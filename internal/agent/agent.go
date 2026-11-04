// Package agent runs the collection and upload loops.
//
// One goroutine collects a sample every collection interval into a bounded in-memory queue. The upload
// loop sends the queue every upload interval (only one request is ever in flight), then drains the disk
// spool oldest-first. When the API is unreachable the queue goes to the spool, and retries back off.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/upsignal/agent/internal/collector"
	"github.com/upsignal/agent/internal/config"
	"github.com/upsignal/agent/internal/spool"
	"github.com/upsignal/agent/internal/state"
	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/transport"
	"github.com/upsignal/agent/internal/version"
)

// ErrRevoked means the API revoked this agent; the process should exit and not be restarted.
var ErrRevoked = errors.New("this agent was revoked in Upsignal; run `sudo upsignal-agent enroll --force` to enroll it again")

const (
	maxStartupJitter = 15 * time.Second
	maxBatchSamples  = 120 // API limit per request
	maxQueuedSamples = 120
	// Spool drain per upload tick; well inside the API's per-agent rate limit.
	maxDrainRequests  = 10
	maxFilesPerDrain  = 100
	minBackoff        = 5 * time.Second
	maxBackoff        = 5 * time.Minute
	unauthorizedRetry = time.Hour
	maxRetryAfter     = time.Hour
	shutdownFlush     = 5 * time.Second
)

type sender interface {
	Telemetry(ctx context.Context, credential string, body []byte) (transport.TelemetryResult, error)
}

type Agent struct {
	cfg       config.Config
	log       *slog.Logger
	stateDir  string
	collector *collector.Collector
	cred      state.Credential
	seq       *state.Sequence
	spool     *spool.Spool
	client    sender
	now       func() time.Time
	jitter    func() time.Duration

	mu     sync.Mutex
	queue  []telemetry.Sample
	host   *telemetry.Host
	health []telemetry.CollectorError

	// Touched only by the collect goroutine.
	lastErrs map[string]string
	// Touched only by the upload loop.
	backoff   time.Duration
	notBefore time.Time
	status    state.Status
}

// New loads the enrollment and local state. It fails with state.ErrNotEnrolled before enrollment.
func New(cfg config.Config, log *slog.Logger, stateDir string) (*Agent, error) {
	cred, err := state.LoadCredential(stateDir)
	if err != nil {
		return nil, err
	}
	if strings.TrimRight(cred.BaseURL, "/") != strings.TrimRight(cfg.API.BaseURL, "/") {
		return nil, fmt.Errorf("enrolled with %s but api.base_url is %s; enroll again with --force",
			cred.BaseURL, cfg.API.BaseURL)
	}
	seq, err := state.OpenSequence(stateDir)
	if err != nil {
		return nil, err
	}
	sp, err := spool.Open(filepath.Join(stateDir, "spool"), time.Duration(cfg.Spool.MaxAge), int64(cfg.Spool.MaxSize))
	if err != nil {
		return nil, err
	}
	client, err := transport.New(cfg.API.BaseURL, time.Duration(cfg.API.RequestTimeout))
	if err != nil {
		return nil, err
	}
	a := newAgent(cfg, log, stateDir, cred, seq, sp, client)
	a.status, _ = state.ReadStatus(stateDir)
	a.status.Revoked = false
	return a, nil
}

func newAgent(cfg config.Config, log *slog.Logger, stateDir string, cred state.Credential,
	seq *state.Sequence, sp *spool.Spool, client sender) *Agent {
	return &Agent{
		cfg: cfg, log: log, stateDir: stateDir, collector: collector.New(),
		cred: cred, seq: seq, spool: sp, client: client,
		now:      time.Now,
		jitter:   func() time.Duration { return rand.N(maxStartupJitter) },
		lastErrs: map[string]string{},
	}
}

// Run collects and uploads until ctx is cancelled, then flushes what it can within a bounded window.
// It returns ErrRevoked if the API revoked the agent.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent starting", "version", version.Version, "commit", version.CommitSHA(),
		"agentId", a.cred.AgentID, "interval", time.Duration(a.cfg.Collection.Interval).String(),
		"uploadInterval", time.Duration(a.cfg.Collection.UploadInterval).String())

	// Prime CPU counters so the first real sample has a CPU value; this reading is not reported.
	a.collector.Collect()
	// Jitter spreads load when a fleet of agents restarts at the same time.
	select {
	case <-ctx.Done():
		a.log.Info("agent stopped")
		return nil
	case <-time.After(a.jitter()):
	}
	host, hostErrs := a.collector.CollectHost()
	a.logCollectorErrors(hostErrs)
	a.mu.Lock()
	a.host = &host
	a.mu.Unlock()
	a.log.Info("host detected", "os", deref(host.OS), "osVersion", deref(host.OSVersion),
		"kernel", deref(host.Kernel), "arch", host.Architecture)

	collectCtx, stopCollect := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.collectLoop(collectCtx)
	}()
	err := a.uploadLoop(ctx)
	stopCollect()
	wg.Wait()
	if errors.Is(err, ErrRevoked) {
		return err
	}
	a.flush()
	a.log.Info("agent stopped")
	return err
}

func (a *Agent) collectLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(a.cfg.Collection.Interval))
	defer ticker.Stop()
	for {
		a.collectOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) collectOnce() {
	s, errs := a.collector.Collect()
	a.logCollectorErrors(errs)
	seq, err := a.seq.Next()
	if err != nil {
		// Without a durable sequence the backend could deduplicate a new sample against an old one.
		a.log.Error("sample skipped", "error", err)
		return
	}
	s.Sequence = seq
	a.mu.Lock()
	defer a.mu.Unlock()
	a.health = errs
	a.queue = append(a.queue, s)
	if over := len(a.queue) - maxQueuedSamples; over > 0 {
		a.queue = a.queue[over:]
		a.log.Warn("dropped queued samples", "count", over)
	}
}

func (a *Agent) takeQueue() []telemetry.Sample {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.queue
	a.queue = nil
	return q
}

func (a *Agent) uploadLoop(ctx context.Context) error {
	interval := time.Duration(a.cfg.Collection.UploadInterval)
	// The first upload happens right away so the server sees the agent as soon as it starts.
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if err := a.tick(ctx); errors.Is(err, ErrRevoked) {
			return err
		}
		wait := interval
		if until := a.notBefore.Sub(a.now()); until > 0 && until < wait {
			wait = until
		}
		timer.Reset(wait)
	}
}

// tick sends the current queue, then drains the spool. While backing off it only spools the queue.
// The only error it returns is ErrRevoked; other failures are handled by retrying later.
func (a *Agent) tick(ctx context.Context) error {
	defer a.writeStatus()
	if a.now().Before(a.notBefore) {
		a.spoolSamples(a.takeQueue())
		return nil
	}
	err := a.deliver(ctx, a.takeQueue(), nil)
	if err == nil {
		err = a.drainSpool(ctx)
	}
	if errors.Is(err, errDeferred) {
		return nil
	}
	return err
}

func (a *Agent) drainSpool(ctx context.Context) error {
	for range maxDrainRequests {
		entries, err := a.spool.List()
		if err != nil {
			a.log.Error("read spool", "error", err)
			return nil
		}
		if len(entries) == 0 {
			return nil
		}
		// Merge small files into one request of at most maxBatchSamples.
		var samples []telemetry.Sample
		var used []spool.Entry
		for _, e := range entries[:min(len(entries), maxFilesPerDrain)] {
			s, err := a.spool.Read(e)
			if err != nil {
				a.log.Warn("removing unreadable spool file", "file", e.Name, "error", err)
				_ = a.spool.Remove(e)
				continue
			}
			if len(used) > 0 && len(samples)+len(s) > maxBatchSamples {
				break
			}
			samples = append(samples, s...)
			used = append(used, e)
		}
		if len(used) == 0 {
			continue
		}
		if err := a.deliver(ctx, samples, used); err != nil {
			return err
		}
	}
	return nil
}

var errDeferred = errors.New("upload deferred")

// deliver sends samples in payload-sized chunks. spooled lists the spool files they came from (nil for the
// current queue). On a transient failure the current queue is spooled and spooled files are kept; the
// backend deduplicates by sequence, so a partly sent batch can safely be sent again.
func (a *Agent) deliver(ctx context.Context, samples []telemetry.Sample, spooled []spool.Entry) error {
	for _, c := range a.chunks(samples) {
		res, err := a.client.Telemetry(ctx, a.cred.Secret, c.body)
		if err == nil {
			if res.Rejected > 0 && !res.Paused {
				a.log.Warn("API rejected samples outside its time window; check the system clock",
					"count", res.Rejected)
			}
			continue
		}
		switch cat := transport.Classify(err); cat {
		case transport.InvalidPayload, transport.TooLarge:
			// Retrying the same payload cannot succeed.
			a.log.Warn("API rejected samples; dropping them", "count", len(c.samples), "category", cat, "error", err)
			a.recordError(cat)
		case transport.Revoked:
			a.log.Error("agent credential revoked", "agentId", a.cred.AgentID)
			if spooled == nil {
				a.spoolSamples(samples)
			}
			a.status.Revoked = true
			a.recordError(cat)
			return ErrRevoked
		default:
			if spooled == nil {
				a.spoolSamples(samples)
			}
			a.retryLater(err, cat)
			return errDeferred
		}
	}
	for _, e := range spooled {
		if err := a.spool.Remove(e); err != nil {
			a.log.Error("remove spool file", "file", e.Name, "error", err)
		}
	}
	now := a.now()
	a.backoff, a.notBefore = 0, time.Time{}
	a.status.LastUploadAt = &now
	return nil
}

type chunk struct {
	samples []telemetry.Sample
	body    []byte
}

// chunks splits samples into requests of at most maxBatchSamples and MaxPayloadBytes. An empty input
// still yields one (empty) request, because the request itself is the liveness heartbeat.
func (a *Agent) chunks(samples []telemetry.Sample) []chunk {
	a.mu.Lock()
	host := a.host
	var health *telemetry.AgentHealth
	if len(a.health) > 0 {
		health = &telemetry.AgentHealth{CollectorErrors: a.health}
	}
	a.mu.Unlock()
	encode := func(s []telemetry.Sample) ([]byte, error) {
		return transport.EncodeBatch(telemetry.Batch{
			AgentVersion: version.Version, SentAt: a.now().UTC(), Host: host,
			Samples: s, AgentHealth: health,
		})
	}
	var out []chunk
	var split func(s []telemetry.Sample)
	split = func(s []telemetry.Sample) {
		body, err := encode(s)
		switch {
		case err == nil:
			out = append(out, chunk{s, body})
		case len(s) > 1:
			split(s[:len(s)/2])
			split(s[len(s)/2:])
		default:
			a.log.Warn("dropping sample larger than the payload limit", "sequence", s[0].Sequence)
		}
	}
	if len(samples) == 0 {
		split([]telemetry.Sample{})
	}
	for start := 0; start < len(samples); start += maxBatchSamples {
		split(samples[start:min(start+maxBatchSamples, len(samples))])
	}
	return out
}

func (a *Agent) retryLater(err error, cat transport.Category) {
	var delay time.Duration
	var apiErr *transport.APIError
	switch {
	case cat == transport.RateLimited && errors.As(err, &apiErr) && apiErr.RetryAfter > 0:
		delay = min(apiErr.RetryAfter, maxRetryAfter)
	case cat == transport.Unauthorized:
		// Usually needs a human (wrong server, deleted record); keep trying, but rarely.
		delay = unauthorizedRetry
	default:
		a.backoff = min(max(a.backoff*2, minBackoff), maxBackoff)
		// Equal jitter: at least half the backoff, so a fleet does not retry in lockstep.
		delay = a.backoff/2 + rand.N(a.backoff/2+1)
	}
	a.notBefore = a.now().Add(delay)
	a.recordError(cat)
	level := slog.LevelWarn
	if cat == transport.Unauthorized {
		level = slog.LevelError
	}
	a.log.Log(context.Background(), level, "upload failed; will retry",
		"category", cat, "retryIn", delay.Round(time.Second).String(), "error", err)
}

func (a *Agent) recordError(cat transport.Category) {
	now := a.now()
	a.status.LastErrorAt = &now
	a.status.LastErrorCategory = string(cat)
}

func (a *Agent) spoolSamples(samples []telemetry.Sample) {
	if len(samples) == 0 {
		return
	}
	if !a.cfg.Spool.Enabled {
		a.log.Warn("spool disabled; dropping samples that could not be sent", "count", len(samples))
		return
	}
	if err := a.spool.Put(samples); err != nil {
		a.log.Error("could not spool samples; they are lost", "count", len(samples), "error", err)
		return
	}
	if dropped, err := a.spool.Prune(); err != nil {
		a.log.Error("prune spool", "error", err)
	} else if dropped > 0 {
		a.log.Warn("spool limit reached; dropped oldest batches", "files", dropped)
	}
}

// flush tries one last upload of the queue within shutdownFlush, and spools whatever is left.
func (a *Agent) flush() {
	samples := a.takeQueue()
	if len(samples) == 0 {
		return
	}
	if a.now().Before(a.notBefore) {
		a.spoolSamples(samples)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlush)
	defer cancel()
	_ = a.deliver(ctx, samples, nil)
	a.writeStatus()
}

func (a *Agent) writeStatus() {
	a.status.UpdatedAt = a.now()
	if entries, err := a.spool.List(); err == nil {
		a.status.SpooledFiles = len(entries)
	}
	if err := state.WriteStatus(a.stateDir, a.status); err != nil {
		a.log.Warn("write status", "error", err)
	}
}

func (a *Agent) logCollectorErrors(errs []telemetry.CollectorError) {
	seen := map[string]bool{}
	for _, e := range errs {
		seen[e.Collector] = true
		if a.lastErrs[e.Collector] != e.Message {
			a.log.Warn("collector error", "collector", e.Collector, "error", e.Message)
			a.lastErrs[e.Collector] = e.Message
		}
	}
	for name := range a.lastErrs {
		if !seen[name] && name != "host" {
			a.log.Info("collector recovered", "collector", name)
			delete(a.lastErrs, name)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
