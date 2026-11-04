package collector

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/upsignal/agent/internal/telemetry"
)

// cpuTimes are the aggregate jiffy counters from the "cpu" line of /proc/stat.
// guest/guest_nice are already included in user/nice, so they are not counted again.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTimes) fields() [8]uint64 {
	return [8]uint64{t.user, t.nice, t.system, t.idle, t.iowait, t.irq, t.softirq, t.steal}
}

func cpuTimesFrom(v [8]uint64) cpuTimes {
	return cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
}

func (t cpuTimes) total() (sum uint64) {
	for _, v := range t.fields() {
		sum += v
	}
	return sum
}

// parseCPUStat returns the aggregate counters and the number of per-CPU lines (logical CPUs).
func parseCPUStat(data []byte) (cpuTimes, int, error) {
	var t cpuTimes
	found, logical := false, 0
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			logical++
			continue
		}
		if len(f) < 9 {
			return t, 0, fmt.Errorf("/proc/stat: cpu line has %d fields, want at least 9", len(f))
		}
		var v [8]uint64
		for i := range v {
			n, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return t, 0, fmt.Errorf("/proc/stat: %w", err)
			}
			v[i] = n
		}
		t = cpuTimesFrom(v)
		found = true
	}
	if !found {
		return t, 0, errors.New("/proc/stat: no aggregate cpu line")
	}
	return t, logical, nil
}

// cpuTracker turns cumulative counters into utilization over the interval between calls.
type cpuTracker struct {
	prev *cpuTimes
}

// update returns nil when there is no usable previous reading: the first call, or counters that went backwards.
// A nil CPU is reported as null rather than a fabricated zero.
func (t *cpuTracker) update(cur cpuTimes) *telemetry.CPU {
	prev := t.prev
	t.prev = &cur
	if prev == nil {
		return nil
	}
	pf, cf := prev.fields(), cur.fields()
	var df [8]uint64
	for i := range cf {
		if cf[i] < pf[i] {
			return nil
		}
		df[i] = cf[i] - pf[i]
	}
	d := cpuTimesFrom(df)
	total := d.total()
	if total == 0 {
		return nil
	}
	return &telemetry.CPU{
		// iowait is idle time spent waiting on I/O, so it does not count as busy.
		UsagePercent:  percent(total-d.idle-d.iowait, total),
		UserPercent:   percent(d.user+d.nice, total),
		SystemPercent: percent(d.system+d.irq+d.softirq, total),
		IOWaitPercent: percent(d.iowait, total),
		StealPercent:  percent(d.steal, total),
	}
}

func (c *Collector) collectCPU() (*telemetry.CPU, error) {
	data, err := c.read("proc/stat")
	if err != nil {
		return nil, err
	}
	cur, _, err := parseCPUStat(data)
	if err != nil {
		return nil, err
	}
	return c.cpu.update(cur), nil
}
