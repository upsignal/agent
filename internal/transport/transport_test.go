package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/upsignal/agent/internal/telemetry"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, 5*time.Second, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c, srv
}

func TestNewRequiresHTTPS(t *testing.T) {
	for _, u := range []string{"http://api.example.com", "api.example.com", "https://"} {
		if _, err := New(u, time.Second); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		}
	}
}

func TestEnroll(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/enroll" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.EnrollmentToken != "tok" || !strings.HasPrefix(r.UserAgent(), "upsignal-agent/") {
			t.Errorf("body = %+v, ua = %q", body, r.UserAgent())
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"agentId":"a1","monitorId":"m1","credential":"c1"}`)
	})
	res, err := c.Enroll(context.Background(), EnrollRequest{EnrollmentToken: "tok"})
	if err != nil || res != (EnrollResult{"a1", "m1", "c1"}) {
		t.Fatalf("Enroll = %+v, %v", res, err)
	}
}

func TestTelemetrySendsBearerCredential(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.RawQuery != "" {
			t.Errorf("auth = %q, query = %q", r.Header.Get("Authorization"), r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"receivedAt":"2026-11-04T15:00:00Z","accepted":2,"duplicates":1,"rejected":0,"paused":false}`)
	})
	body, err := EncodeBatch(telemetry.Batch{AgentVersion: "1", Samples: []telemetry.Sample{}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Telemetry(context.Background(), "secret", body)
	if err != nil || res.Accepted != 2 || res.Duplicates != 1 {
		t.Fatalf("Telemetry = %+v, %v", res, err)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		status     int
		body       string
		retryAfter string
		want       Category
		wantDelay  time.Duration
	}{
		{400, `{"code":"INVALID_TELEMETRY","message":"bad"}`, "", InvalidPayload, 0},
		{401, `{"code":"INVALID_AGENT_CREDENTIAL"}`, "", Unauthorized, 0},
		{403, `{"code":"AGENT_CREDENTIAL_REVOKED"}`, "", Revoked, 0},
		{403, `{"code":"SOMETHING_ELSE"}`, "", Unexpected, 0},
		{413, ``, "", TooLarge, 0},
		{429, `{"code":"AGENT_RATE_LIMITED"}`, "42", RateLimited, 42 * time.Second},
		{502, `<html>bad gateway</html>`, "", Server, 0},
		{404, ``, "", Unexpected, 0},
	}
	for _, tc := range tests {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.retryAfter != "" {
				w.Header().Set("Retry-After", tc.retryAfter)
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := c.Telemetry(context.Background(), "secret", []byte(`{}`))
		if got := Classify(err); got != tc.want {
			t.Errorf("%d %s: category = %s, want %s", tc.status, tc.body, got, tc.want)
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter != tc.wantDelay {
			t.Errorf("%d: RetryAfter = %s, want %s", tc.status, apiErr.RetryAfter, tc.wantDelay)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error leaks the credential: %v", err)
		}
	}
}

func TestNetworkErrorsAreRetryable(t *testing.T) {
	c, srv := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	srv.Close()
	_, err := c.Telemetry(context.Background(), "secret", []byte(`{}`))
	if Classify(err) != Network {
		t.Fatalf("closed server: category = %s (%v)", Classify(err), err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	// Use the production client settings, but trust the test certificate.
	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = srv.Client().Transport
	_, err = c.Telemetry(context.Background(), "secret", []byte(`{}`))
	if Classify(err) != Unexpected || hits.Load() != 0 {
		t.Fatalf("redirect: category = %s, target hits = %d; the credential must not follow redirects",
			Classify(err), hits.Load())
	}
}

func TestEncodeBatchEnforcesPayloadLimit(t *testing.T) {
	big := telemetry.Batch{AgentVersion: strings.Repeat("x", MaxPayloadBytes)}
	if _, err := EncodeBatch(big); Classify(err) != TooLarge {
		t.Fatalf("oversized batch: %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("Wed, 04 Nov 2026 15:01:00 GMT", now); got != time.Minute {
		t.Errorf("HTTP date = %s", got)
	}
	for _, v := range []string{"", "soon", "-5"} {
		if got := parseRetryAfter(v, now); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %s, want 0", v, got)
		}
	}
}
