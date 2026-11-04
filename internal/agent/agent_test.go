package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/upsignal/agent/internal/config"
	"github.com/upsignal/agent/internal/spool"
	"github.com/upsignal/agent/internal/state"
	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/transport"
)

// fakeSender records every request and answers with the next scripted error (nil = success).
type fakeSender struct {
	requests [][]uint64
	errs     []error
}

func (f *fakeSender) Telemetry(_ context.Context, credential string, body []byte) (transport.TelemetryResult, error) {
	if credential != "secret" {
		return transport.TelemetryResult{}, errors.New("wrong credential")
	}
	var b telemetry.Batch
	if err := json.Unmarshal(body, &b); err != nil {
		return transport.TelemetryResult{}, err
	}
	seqs := []uint64{}
	for _, s := range b.Samples {
		seqs = append(seqs, s.Sequence)
	}
	f.requests = append(f.requests, seqs)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return transport.TelemetryResult{}, err
		}
	}
	return transport.TelemetryResult{Accepted: len(seqs)}, nil
}

type harness struct {
	*Agent
	fake *fakeSender
	now  time.Time
	next uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	seq, err := state.OpenSequence(dir)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(dir, "spool"), 24*time.Hour, 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{fake: &fakeSender{}, now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	cfg := config.Default()
	cfg.API.BaseURL = "https://api.example.com"
	h.Agent = newAgent(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), dir,
		state.Credential{AgentID: "a1", Secret: "secret"}, seq, sp, h.fake)
	h.Agent.now = func() time.Time { return h.now }
	return h
}

// enqueue adds n samples with consecutive sequence numbers to the in-memory queue.
func (h *harness) enqueue(n int) {
	for range n {
		h.queue = append(h.queue, telemetry.Sample{Sequence: h.next, ObservedAt: h.now})
		h.next++
	}
}

func (h *harness) spooled(t *testing.T) int {
	t.Helper()
	entries, err := h.spool.List()
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

var (
	networkErr = errors.New("dial tcp: connection refused")
	revokedErr = &transport.APIError{Status: 403, Code: "AGENT_CREDENTIAL_REVOKED"}
)

func TestEmptyQueueStillSendsHeartbeat(t *testing.T) {
	h := newHarness(t)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.fake.requests) != 1 || len(h.fake.requests[0]) != 0 {
		t.Fatalf("requests = %v, want one empty batch", h.fake.requests)
	}
	st, _ := state.ReadStatus(h.stateDir)
	if st.LastUploadAt == nil {
		t.Fatal("status should record the upload")
	}
}

func TestLargeQueueIsChunked(t *testing.T) {
	h := newHarness(t)
	h.enqueue(250)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.fake.requests); n != 3 || len(h.fake.requests[0]) != maxBatchSamples || len(h.fake.requests[2]) != 10 {
		t.Fatalf("got %d requests, sizes %d/%d", n, len(h.fake.requests[0]), len(h.fake.requests[n-1]))
	}
}

func TestOutageSpoolsThenReplaysInOrder(t *testing.T) {
	h := newHarness(t)
	h.fake.errs = []error{networkErr}
	h.enqueue(3)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.spooled(t) != 1 || h.notBefore.IsZero() {
		t.Fatalf("after failure: spooled=%d notBefore=%v", h.spooled(t), h.notBefore)
	}
	firstBackoff := h.notBefore.Sub(h.now)
	if firstBackoff < minBackoff/2 || firstBackoff > minBackoff {
		t.Fatalf("first backoff %s outside [%s, %s]", firstBackoff, minBackoff/2, minBackoff)
	}

	// While backing off, ticks do not call the API; new samples go to the spool.
	h.enqueue(2)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.fake.requests) != 1 || h.spooled(t) != 2 {
		t.Fatalf("during backoff: requests=%d spooled=%d", len(h.fake.requests), h.spooled(t))
	}

	// After the backoff the queue is sent, then the spool drains merged, oldest first.
	h.now = h.notBefore
	h.enqueue(1)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]uint64{{0, 1, 2}, {5}, {0, 1, 2, 3, 4}}
	if !equalRequests(h.fake.requests, want) {
		t.Fatalf("requests = %v, want %v", h.fake.requests, want)
	}
	if h.spooled(t) != 0 || h.backoff != 0 || !h.notBefore.IsZero() {
		t.Fatalf("after recovery: spooled=%d backoff=%s", h.spooled(t), h.backoff)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	h := newHarness(t)
	for range 20 {
		h.retryLater(networkErr, transport.Network)
	}
	if h.backoff != maxBackoff {
		t.Fatalf("backoff = %s, want cap %s", h.backoff, maxBackoff)
	}
	if d := h.notBefore.Sub(h.now); d < maxBackoff/2 || d > maxBackoff {
		t.Fatalf("delay %s outside jitter range", d)
	}
}

func TestRetryAfterAndUnauthorizedDelays(t *testing.T) {
	h := newHarness(t)
	h.retryLater(&transport.APIError{Status: 429, RetryAfter: 42 * time.Second}, transport.RateLimited)
	if d := h.notBefore.Sub(h.now); d != 42*time.Second {
		t.Fatalf("429 delay = %s, want Retry-After", d)
	}
	h.retryLater(&transport.APIError{Status: 429, RetryAfter: 24 * time.Hour}, transport.RateLimited)
	if d := h.notBefore.Sub(h.now); d != maxRetryAfter {
		t.Fatalf("huge Retry-After = %s, want capped at %s", d, maxRetryAfter)
	}
	h.retryLater(&transport.APIError{Status: 401}, transport.Unauthorized)
	if d := h.notBefore.Sub(h.now); d != unauthorizedRetry {
		t.Fatalf("401 delay = %s", d)
	}
}

func TestSpoolFailureDuringDrainKeepsFiles(t *testing.T) {
	h := newHarness(t)
	h.enqueue(2)
	h.spoolSamples(h.takeQueue())
	h.fake.errs = []error{nil, networkErr} // heartbeat succeeds, the spool drain fails
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.spooled(t) != 1 {
		t.Fatalf("spooled = %d; a failed drain must keep its file", h.spooled(t))
	}
}

func TestInvalidPayloadIsDropped(t *testing.T) {
	h := newHarness(t)
	h.fake.errs = []error{&transport.APIError{Status: 400, Code: "INVALID_TELEMETRY"}}
	h.enqueue(2)
	if err := h.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.spooled(t) != 0 || !h.notBefore.IsZero() {
		t.Fatal("an invalid payload must not be spooled or retried")
	}
}

func TestRevokedStopsAndKeepsSamples(t *testing.T) {
	h := newHarness(t)
	h.fake.errs = []error{revokedErr}
	h.enqueue(2)
	if err := h.tick(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}
	st, _ := state.ReadStatus(h.stateDir)
	if !st.Revoked || h.spooled(t) != 1 {
		t.Fatalf("status = %+v, spooled = %d", st, h.spooled(t))
	}
}

func TestSpoolDisabledDropsSamples(t *testing.T) {
	h := newHarness(t)
	h.cfg.Spool.Enabled = false
	h.fake.errs = []error{networkErr}
	h.enqueue(2)
	_ = h.tick(context.Background())
	if h.spooled(t) != 0 {
		t.Fatal("spool.enabled=false must not write to disk")
	}
}

func TestQueueIsBounded(t *testing.T) {
	h := newHarness(t)
	for range maxQueuedSamples + 5 {
		h.collectOnce()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) != maxQueuedSamples || h.queue[0].Sequence != 5 {
		t.Fatalf("queue = %d samples from sequence %d; want the newest %d", len(h.queue), h.queue[0].Sequence, maxQueuedSamples)
	}
}

func TestRunStopsOnCancelAndFlushes(t *testing.T) {
	h := newHarness(t)
	h.jitter = func() time.Duration { return 0 }
	h.Agent.now = time.Now
	h.cfg.Collection.Interval = config.Duration(10 * time.Millisecond)
	h.cfg.Collection.UploadInterval = config.Duration(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := h.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// The first request is the immediate startup upload; the last is the shutdown flush.
	if len(h.fake.requests) < 2 {
		t.Fatalf("requests = %v, want startup upload and shutdown flush", h.fake.requests)
	}
	var seen []uint64
	for _, r := range h.fake.requests {
		seen = append(seen, r...)
	}
	for i, s := range seen {
		if s != uint64(i) {
			t.Fatalf("sequences %v are not contiguous from 0", seen)
		}
	}
}

func equalRequests(got, want [][]uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				return false
			}
		}
	}
	return true
}
