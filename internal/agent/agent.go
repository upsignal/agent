// Package agent runs the collection loop.
//
// Telemetry delivery (batching, spool, HTTPS upload) is milestone M4; until then samples are only logged.
package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/upsignal/agent/internal/collector"
	"github.com/upsignal/agent/internal/config"
	"github.com/upsignal/agent/internal/telemetry"
	"github.com/upsignal/agent/internal/version"
)

const maxStartupJitter = 15 * time.Second

type Agent struct {
	cfg       config.Config
	log       *slog.Logger
	collector *collector.Collector
	// lastErrs remembers each collector's last error so a persistent failure is logged once, not every interval.
	lastErrs map[string]string
}

func New(cfg config.Config, log *slog.Logger) *Agent {
	return &Agent{cfg: cfg, log: log, collector: collector.New(), lastErrs: map[string]string{}}
}

// Run collects until ctx is cancelled (SIGTERM/SIGINT) and returns nil on a clean shutdown.
func (a *Agent) Run(ctx context.Context) error {
	interval := time.Duration(a.cfg.Collection.Interval)
	a.log.Info("agent starting", "version", version.Version, "commit", version.CommitSHA(), "interval", interval.String())

	// Prime CPU counters right away so the first reported sample after the jitter has a CPU value.
	a.collect()

	// Jitter spreads load when a fleet of agents restarts at the same time.
	jitter := rand.N(maxStartupJitter)
	select {
	case <-ctx.Done():
		a.log.Info("agent stopped")
		return nil
	case <-time.After(jitter):
	}

	host, hostErrs := a.collector.CollectHost()
	a.logErrors(hostErrs)
	a.log.Info("host detected", "os", deref(host.OS), "osVersion", deref(host.OSVersion),
		"kernel", deref(host.Kernel), "arch", host.Architecture)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.collect()
		select {
		case <-ctx.Done():
			a.log.Info("agent stopped")
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Agent) collect() telemetry.Sample {
	s, errs := a.collector.Collect()
	a.logErrors(errs)
	a.log.Debug("sample collected", "observedAt", s.ObservedAt, "cpu", s.CPU != nil,
		"filesystems", len(s.Filesystems), "interfaces", len(s.NetworkInterfaces))
	return s
}

func (a *Agent) logErrors(errs []telemetry.CollectorError) {
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
