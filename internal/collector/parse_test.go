package collector

import (
	"testing"

	"github.com/upsignal/agent/internal/telemetry"
)

func TestParseCPUStat(t *testing.T) {
	data := []byte("cpu  100 5 50 800 20 3 2 10 7 0\ncpu0 50 2 25 400 10 1 1 5 0 0\ncpu1 50 3 25 400 10 2 1 5 0 0\nintr 12345\n")
	got, logical, err := parseCPUStat(data)
	if err != nil {
		t.Fatal(err)
	}
	want := cpuTimes{user: 100, nice: 5, system: 50, idle: 800, iowait: 20, irq: 3, softirq: 2, steal: 10}
	if got != want || logical != 2 {
		t.Fatalf("got %+v, %d cpus; want %+v, 2", got, logical, want)
	}
	if _, _, err := parseCPUStat([]byte("cpu 1 2 3\n")); err == nil {
		t.Error("short cpu line: want error")
	}
	if _, _, err := parseCPUStat([]byte("intr 1\n")); err == nil {
		t.Error("no cpu line: want error")
	}
}

func TestCPUTrackerDeltas(t *testing.T) {
	var tr cpuTracker
	if got := tr.update(cpuTimes{user: 100, idle: 900}); got != nil {
		t.Fatalf("first sample = %+v, want nil (no fabricated zero)", got)
	}
	// Over this interval: 1000 jiffies total; 600 idle, 100 iowait, 150 user, 50 nice, 50 system, 50 steal.
	got := tr.update(cpuTimes{user: 250, nice: 50, system: 50, idle: 1500, iowait: 100, steal: 50})
	want := &telemetry.CPU{UsagePercent: 30, UserPercent: 20, SystemPercent: 5, IOWaitPercent: 10, StealPercent: 5}
	if got == nil || *got != *want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Counters going backwards (e.g. a reset) must not produce a bogus reading, and must re-baseline.
	if got := tr.update(cpuTimes{user: 10, idle: 10}); got != nil {
		t.Fatalf("after reset = %+v, want nil", got)
	}
	if got := tr.update(cpuTimes{user: 10, idle: 10}); got != nil {
		t.Fatalf("zero delta = %+v, want nil", got)
	}
	if got := tr.update(cpuTimes{user: 60, idle: 60}); got == nil || got.UsagePercent != 50 {
		t.Fatalf("after re-baseline = %+v, want 50%% usage", got)
	}
}

func TestMemoryFromMeminfo(t *testing.T) {
	m, err := parseMeminfo([]byte(`MemTotal:        8000000 kB
MemFree:          500000 kB
MemAvailable:    2000000 kB
Buffers:          100000 kB
Cached:          1000000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
HugePages_Total:       0
`))
	if err != nil {
		t.Fatal(err)
	}
	if m["HugePages_Total"] != 0 || m["MemTotal"] != 8000000*1024 {
		t.Fatalf("unit handling wrong: %v", m)
	}
	mem, err := memoryFromMeminfo(m)
	if err != nil {
		t.Fatal(err)
	}
	if mem.AvailableBytes != 2000000*1024 || mem.UsedBytes != 6000000*1024 || mem.UsedPercent != 75 {
		t.Errorf("memory = %+v; want MemAvailable used, not MemFree", mem)
	}
	if mem.SwapUsedBytes != 500000*1024 || mem.SwapUsedPercent == nil || *mem.SwapUsedPercent != 25 {
		t.Errorf("swap = %+v", mem)
	}

	noSwap, _ := memoryFromMeminfo(map[string]uint64{"MemTotal": 100, "MemAvailable": 50})
	if noSwap.SwapUsedPercent != nil {
		t.Errorf("no swap: percent = %v, want nil", *noSwap.SwapUsedPercent)
	}
	if _, err := memoryFromMeminfo(map[string]uint64{"MemFree": 1}); err == nil {
		t.Error("missing MemTotal: want error")
	}
}

func TestParseLoadAndUptime(t *testing.T) {
	load, err := parseLoadavg([]byte("0.82 0.74 0.61 2/345 6789\n"))
	if err != nil || *load != (telemetry.Load{OneMinute: 0.82, FiveMinutes: 0.74, FifteenMinutes: 0.61}) {
		t.Fatalf("load = %+v, %v", load, err)
	}
	if up, err := parseUptime([]byte("864000.57 1700000.12\n")); err != nil || up != 864000 {
		t.Fatalf("uptime = %d, %v", up, err)
	}
	if _, err := parseUptime([]byte("x")); err == nil {
		t.Error("bad uptime: want error")
	}
}

func TestParseNetDev(t *testing.T) {
	data := []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000      50    0    0    0     0          0         0     5000      50    0    0    0     0       0          0
  eth0: 1000000   900   1    2    0     0          0         0   2000000    800    3    4    0     0       0          0
  ens4: 10 1 0 0 0 0 0 0 20 2 0 0 0 0 0 0
  sit0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
 tunl0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
veth1a2b: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0
`)
	got, err := parseNetDev(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "ens4" || got[1].Name != "eth0" {
		t.Fatalf("interfaces = %+v; want ens4, eth0 sorted, loopback/tunnel/veth excluded", got)
	}
	want := telemetry.NetworkInterface{Name: "eth0", RxBytes: 1000000, RxPackets: 900, RxErrors: 1, RxDropped: 2,
		TxBytes: 2000000, TxPackets: 800, TxErrors: 3, TxDropped: 4}
	if got[1] != want {
		t.Fatalf("eth0 = %+v, want %+v", got[1], want)
	}
}

func TestParseOSRelease(t *testing.T) {
	m := parseOSRelease([]byte("# comment\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nVERSION_ID='24.04'\n\n"))
	if m["ID"] != "ubuntu" || m["VERSION_ID"] != "24.04" || m["PRETTY_NAME"] != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("got %v", m)
	}
}
