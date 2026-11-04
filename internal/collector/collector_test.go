package collector

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRoot writes a minimal /proc and /etc tree into a temp dir.
func fakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func baseFiles() map[string]string {
	return map[string]string{
		"proc/stat":                      "cpu  100 0 50 800 0 0 0 0 0 0\ncpu0 100 0 50 800 0 0 0 0 0 0\n",
		"proc/meminfo":                   "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"proc/loadavg":                   "0.10 0.20 0.30 1/100 42\n",
		"proc/uptime":                    "3600.5 100.0\n",
		"proc/sys/kernel/random/boot_id": "6f1d2c3b-1111-4a2b-9c3d-0123456789ab\n",
		"proc/sys/kernel/osrelease":      "6.8.0-45-generic\n",
		"proc/self/mountinfo": "22 1 252:1 / / rw - ext4 /dev/vda1 rw\n" +
			"23 22 252:2 /lib/docker/hosts /etc/hosts rw - ext4 /dev/vdb rw\n",
		"etc/hosts":      "127.0.0.1 localhost\n",
		"proc/net/dev":   "h1\nh2\n  eth0: 1 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n",
		"etc/os-release": "ID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n",
	}
}

func newTestCollector(root string) *Collector {
	return &Collector{
		root: root,
		statfs: func(string) (fsStats, error) {
			return fsStats{blocks: 10, bfree: 5, bavail: 5, files: 10, ffree: 5, bsize: 4096}, nil
		},
		hostname: func() (string, error) { return "production-01", nil },
		now:      func() time.Time { return time.Date(2026, 11, 4, 15, 0, 0, 123456789, time.FixedZone("CET", 3600)) },
	}
}

func TestCollect(t *testing.T) {
	c := newTestCollector(fakeRoot(t, baseFiles()))
	s, errs := c.Collect()
	if len(errs) != 0 {
		t.Fatalf("errors: %+v", errs)
	}
	if s.ObservedAt.Location() != time.UTC || s.ObservedAt.Nanosecond() != 123000000 {
		t.Errorf("observedAt = %s; want UTC truncated to ms", s.ObservedAt)
	}
	if s.CPU != nil {
		t.Errorf("first CPU sample = %+v, want nil", s.CPU)
	}
	if s.Memory.UsedPercent != 60 || *s.UptimeSeconds != 3600 || s.Load.FifteenMinutes != 0.3 {
		t.Errorf("sample = %+v", s)
	}
	if len(s.Filesystems) != 1 || len(s.NetworkInterfaces) != 1 || *s.BootID != "6f1d2c3b-1111-4a2b-9c3d-0123456789ab" {
		t.Errorf("sample = %+v", s)
	}

	h, errs := c.CollectHost()
	if len(errs) != 0 {
		t.Fatalf("host errors: %+v", errs)
	}
	if *h.Hostname != "production-01" || *h.OS != "debian" || *h.OSVersion != "12" ||
		*h.Kernel != "6.8.0-45-generic" || *h.LogicalCPUs != 1 || h.Architecture == "" {
		t.Errorf("host = %+v", h)
	}
}

func TestCollectReportsFailuresAsNull(t *testing.T) {
	files := baseFiles()
	delete(files, "proc/meminfo")
	files["proc/loadavg"] = "garbage"
	c := newTestCollector(fakeRoot(t, files))
	c.statfs = func(string) (fsStats, error) { return fsStats{}, errors.New("permission denied") }

	s, errs := c.Collect()
	got := map[string]bool{}
	for _, e := range errs {
		got[e.Collector] = true
	}
	for _, name := range []string{"memory", "load", "disk"} {
		if !got[name] {
			t.Errorf("missing %s error in %+v", name, errs)
		}
	}
	if s.UptimeSeconds == nil || s.NetworkInterfaces == nil {
		t.Error("healthy collectors must still report")
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Failed collectors are null, not zero; a disk collector that ran but statfs'd nothing is an empty list.
	for _, want := range []string{`"memory":null`, `"load":null`, `"cpu":null`, `"filesystems":[]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("JSON %s missing %s", out, want)
		}
	}
}

func TestCollectRecoversFromPanic(t *testing.T) {
	c := newTestCollector(fakeRoot(t, baseFiles()))
	c.statfs = func(string) (fsStats, error) { panic("boom") }
	s, errs := c.Collect()
	if len(errs) != 1 || errs[0].Collector != "disk" || !strings.Contains(errs[0].Message, "boom") {
		t.Fatalf("errors = %+v", errs)
	}
	if s.Memory == nil || s.NetworkInterfaces == nil {
		t.Error("a panicking collector must not stop the others")
	}
}
