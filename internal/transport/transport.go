// Package transport is the HTTPS client for the Upsignal agent API (POST /v1/agents/enroll and
// POST /v1/agents/telemetry). TLS verification is always on, redirects are never followed (so the
// credential cannot leak to another host), and secrets only travel in request bodies or headers.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/version"
)

// MaxPayloadBytes is the API's body limit for telemetry.
const MaxPayloadBytes = 256 << 10

const maxResponseBytes = 64 << 10

// Category says how the caller should react to a failed request.
type Category string

const (
	Network        Category = "network"         // DNS, connect, TLS or timeout: retry with backoff
	Server         Category = "server"          // 5xx: retry with backoff
	RateLimited    Category = "rate_limited"    // 429: wait Retry-After
	Unauthorized   Category = "unauthorized"    // 401: retry slowly; may be a proxy or misconfiguration
	Revoked        Category = "revoked"         // 403 AGENT_CREDENTIAL_REVOKED: stop until re-enrolled
	InvalidPayload Category = "invalid_payload" // 400: do not retry the same payload
	TooLarge       Category = "too_large"       // 413: split the batch
	Unexpected     Category = "unexpected"      // other statuses: retry with backoff
)

// APIError is a non-2xx response.
type APIError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("API returned %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("API returned %d", e.Status)
}

// Classify maps any error from this package to a Category.
func Classify(err error) Category {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return Network
	}
	switch s := apiErr.Status; {
	case s == http.StatusTooManyRequests:
		return RateLimited
	case s == http.StatusUnauthorized:
		return Unauthorized
	case s == http.StatusForbidden && apiErr.Code == "AGENT_CREDENTIAL_REVOKED":
		return Revoked
	case s == http.StatusBadRequest:
		return InvalidPayload
	case s == http.StatusRequestEntityTooLarge:
		return TooLarge
	case s >= 500:
		return Server
	}
	return Unexpected
}

type Client struct {
	base *url.URL
	http *http.Client
}

type Option func(*Client)

// WithHTTPClient replaces the HTTP client; tests use it to trust a local certificate.
func WithHTTPClient(c *http.Client) Option { return func(cl *Client) { cl.http = c } }

func New(baseURL string, timeout time.Duration, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("api base URL must be https, got %q", baseURL)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.Proxy = http.ProxyFromEnvironment
	t.MaxIdleConns, t.MaxIdleConnsPerHost = 2, 1
	c := &Client{base: u, http: &http.Client{
		Transport: t,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func (c *Client) endpoint(path string) string { return c.base.String() + path }

type EnrollRequest struct {
	EnrollmentToken string          `json:"enrollmentToken"`
	AgentVersion    string          `json:"agentVersion"`
	Host            *telemetry.Host `json:"host,omitempty"`
}

type EnrollResult struct {
	AgentID    string `json:"agentId"`
	MonitorID  string `json:"monitorId"`
	Credential string `json:"credential"`
}

type TelemetryResult struct {
	ReceivedAt time.Time `json:"receivedAt"`
	Accepted   int       `json:"accepted"`
	Duplicates int       `json:"duplicates"`
	Rejected   int       `json:"rejected"`
	Paused     bool      `json:"paused"`
}

func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (EnrollResult, error) {
	var out EnrollResult
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	if err := c.do(ctx, "/v1/agents/enroll", "", body, &out); err != nil {
		return out, err
	}
	if out.AgentID == "" || out.Credential == "" {
		return out, errors.New("enrollment response is missing the agent ID or credential")
	}
	return out, nil
}

// EncodeBatch returns the request body, or an error with Category TooLarge if it exceeds MaxPayloadBytes.
func EncodeBatch(b telemetry.Batch) ([]byte, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxPayloadBytes {
		return nil, &APIError{Status: http.StatusRequestEntityTooLarge, Message: "batch exceeds 256 KB"}
	}
	return body, nil
}

func (c *Client) Telemetry(ctx context.Context, credential string, body []byte) (TelemetryResult, error) {
	var out TelemetryResult
	err := c.do(ctx, "/v1/agents/telemetry", credential, body, &out)
	return out, err
}

// Health checks GET /v1/healthz, which needs no credential.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/v1/healthz"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode}
	}
	return nil
}

func (c *Client) do(ctx context.Context, path, credential string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error includes the URL, which never contains secrets here.
		return err
	}
	defer drain(resp.Body)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
		var payload struct {
			Code    string `json:"code"`
			Message any    `json:"message"`
		}
		if json.Unmarshal(data, &payload) == nil {
			apiErr.Code = payload.Code
			if m, ok := payload.Message.(string); ok {
				apiErr.Message = m
			}
		}
		return apiErr
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

func userAgent() string { return "upsignal-agent/" + version.Version }

func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	body.Close()
}

// parseRetryAfter accepts delta-seconds or an HTTP date; it returns 0 when absent or invalid.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
