package collector

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/upsignal/agent/internal/telemetry"
)

func parseLoadavg(data []byte) (*telemetry.Load, error) {
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return nil, errors.New("/proc/loadavg: want at least 3 fields")
	}
	var v [3]float64
	for i := range v {
		n, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return nil, fmt.Errorf("/proc/loadavg: %w", err)
		}
		v[i] = n
	}
	return &telemetry.Load{OneMinute: v[0], FiveMinutes: v[1], FifteenMinutes: v[2]}, nil
}

func parseUptime(data []byte) (int64, error) {
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime: empty")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil || secs < 0 {
		return 0, fmt.Errorf("/proc/uptime: invalid value %q", f[0])
	}
	return int64(secs), nil
}

// parseOSRelease reads KEY=value pairs, stripping optional single or double quotes.
func parseOSRelease(data []byte) map[string]string {
	m := make(map[string]string)
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[k] = v
	}
	return m
}

func (c *Collector) collectLoad() (*telemetry.Load, error) {
	data, err := c.read("proc/loadavg")
	if err != nil {
		return nil, err
	}
	return parseLoadavg(data)
}

func (c *Collector) collectUptime() (*int64, error) {
	data, err := c.read("proc/uptime")
	if err != nil {
		return nil, err
	}
	secs, err := parseUptime(data)
	if err != nil {
		return nil, err
	}
	return &secs, nil
}

// collectBootID reads the random per-boot UUID. Unlike /etc/machine-id it does not identify the machine across boots.
func (c *Collector) collectBootID() (*string, error) {
	data, err := c.read("proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 36 {
		return nil, fmt.Errorf("boot_id: unexpected value length %d", len(id))
	}
	return &id, nil
}
