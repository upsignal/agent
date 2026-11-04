// Package collector reads Linux resource metrics from /proc, /sys and statfs.
//
// Parsing is plain Go over file contents so it can be unit-tested on any OS; only statfs is Linux-specific.
// Collectors are read-only and never execute commands.
package collector

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/upsignal/agent/internal/telemetry"
)

// Caps keep the payload bounded on hosts with many mounts or interfaces.
const (
	MaxFilesystems = 32
	MaxInterfaces  = 32
)

// Collector produces samples. It keeps the previous CPU counters, so it is not safe for concurrent use.
type Collector struct {
	root     string // "/" in production; a fixture directory in tests
	statfs   func(path string) (fsStats, error)
	hostname func() (string, error)
	now      func() time.Time
	cpu      cpuTracker
}

func New() *Collector {
	return &Collector{root: "/", statfs: statfs, hostname: os.Hostname, now: time.Now}
}

func (c *Collector) read(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(c.root, path))
}

// Collect takes one sample. A failing collector leaves its field null and adds an error; the rest still report.
func (c *Collector) Collect() (telemetry.Sample, []telemetry.CollectorError) {
	s := telemetry.Sample{ObservedAt: c.now().UTC().Truncate(time.Millisecond)}
	var errs []telemetry.CollectorError
	run := func(name string, fn func() error) {
		defer func() {
			if r := recover(); r != nil {
				errs = append(errs, telemetry.CollectorError{Collector: name, Message: fmt.Sprintf("panic: %v", r)})
			}
		}()
		if err := fn(); err != nil {
			errs = append(errs, telemetry.CollectorError{Collector: name, Message: err.Error()})
		}
	}

	run("cpu", func() (err error) { s.CPU, err = c.collectCPU(); return })
	run("memory", func() (err error) { s.Memory, err = c.collectMemory(); return })
	run("load", func() (err error) { s.Load, err = c.collectLoad(); return })
	run("uptime", func() (err error) { s.UptimeSeconds, err = c.collectUptime(); return })
	run("boot_id", func() (err error) { s.BootID, err = c.collectBootID(); return })
	run("disk", func() (err error) { s.Filesystems, err = c.collectFilesystems(); return })
	run("network", func() (err error) { s.NetworkInterfaces, err = c.collectNetwork(); return })
	return s, errs
}

// CollectHost reads host metadata. Fields that cannot be read are left null.
func (c *Collector) CollectHost() (telemetry.Host, []telemetry.CollectorError) {
	h := telemetry.Host{Architecture: runtime.GOARCH}
	var errs []telemetry.CollectorError
	add := func(err error) {
		errs = append(errs, telemetry.CollectorError{Collector: "host", Message: err.Error()})
	}

	if name, err := c.hostname(); err != nil {
		add(fmt.Errorf("hostname: %w", err))
	} else {
		h.Hostname = &name
	}
	if osr, err := c.readOSRelease(); err != nil {
		add(err)
	} else {
		h.OS, h.OSName, h.OSVersion = nonEmpty(osr["ID"]), nonEmpty(osr["PRETTY_NAME"]), nonEmpty(osr["VERSION_ID"])
	}
	if data, err := c.read("proc/sys/kernel/osrelease"); err != nil {
		add(fmt.Errorf("kernel release: %w", err))
	} else {
		h.Kernel = nonEmpty(strings.TrimSpace(string(data)))
	}
	if data, err := c.read("proc/stat"); err != nil {
		add(fmt.Errorf("logical cpus: %w", err))
	} else if _, n, err := parseCPUStat(data); err != nil {
		add(fmt.Errorf("logical cpus: %w", err))
	} else {
		h.LogicalCPUs = &n
	}
	return h, errs
}

func (c *Collector) readOSRelease() (map[string]string, error) {
	data, err := c.read("etc/os-release")
	if errors.Is(err, os.ErrNotExist) {
		data, err = c.read("usr/lib/os-release")
	}
	if err != nil {
		return nil, fmt.Errorf("os-release: %w", err)
	}
	return parseOSRelease(data), nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func percent(part, whole uint64) float64 {
	return round2(float64(part) / float64(whole) * 100)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
