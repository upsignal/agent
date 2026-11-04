package collector

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/upsignal/agent/internal/telemetry"
)

// parseMeminfo returns /proc/meminfo values in bytes, keyed by field name.
func parseMeminfo(data []byte) (map[string]uint64, error) {
	m := make(map[string]uint64)
	for line := range strings.Lines(string(data)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("/proc/meminfo %s: %w", key, err)
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		m[key] = n
	}
	return m, nil
}

func memoryFromMeminfo(m map[string]uint64) (*telemetry.Memory, error) {
	total, ok := m["MemTotal"]
	if !ok || total == 0 {
		return nil, errors.New("/proc/meminfo: missing MemTotal")
	}
	// MemAvailable counts reclaimable cache; MemFree alone overstates pressure. The fallback is for very old kernels.
	avail, ok := m["MemAvailable"]
	if !ok {
		avail = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	avail = min(avail, total)
	mem := &telemetry.Memory{
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      total - avail,
		UsedPercent:    percent(total-avail, total),
		SwapTotalBytes: m["SwapTotal"],
		SwapFreeBytes:  min(m["SwapFree"], m["SwapTotal"]),
	}
	mem.SwapUsedBytes = mem.SwapTotalBytes - mem.SwapFreeBytes
	if mem.SwapTotalBytes > 0 {
		p := percent(mem.SwapUsedBytes, mem.SwapTotalBytes)
		mem.SwapUsedPercent = &p
	}
	return mem, nil
}

func (c *Collector) collectMemory() (*telemetry.Memory, error) {
	data, err := c.read("proc/meminfo")
	if err != nil {
		return nil, err
	}
	m, err := parseMeminfo(data)
	if err != nil {
		return nil, err
	}
	return memoryFromMeminfo(m)
}
