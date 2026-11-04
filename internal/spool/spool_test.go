package spool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/upsignal/agent/internal/telemetry"
)

func samples(seqs ...uint64) []telemetry.Sample {
	out := make([]telemetry.Sample, len(seqs))
	for i, s := range seqs {
		out[i] = telemetry.Sample{Sequence: s, ObservedAt: time.Unix(int64(s), 0).UTC()}
	}
	return out
}

func openAt(t *testing.T, now *time.Time, maxAge time.Duration, maxSize int64) (*Spool, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := Open(dir, maxAge, maxSize)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return *now }
	return s, dir
}

func TestPutListReadRemove(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, dir := openAt(t, &now, time.Hour, 1<<20)
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", info.Mode().Perm())
	}
	if err := s.Put(nil); err != nil {
		t.Fatal(err)
	}
	for i, batch := range [][]telemetry.Sample{samples(0, 1), samples(2), samples(3, 4, 5)} {
		now = now.Add(time.Duration(i+1) * time.Second)
		if err := s.Put(batch); err != nil {
			t.Fatal(err)
		}
	}
	// Leftover temp files and foreign files are ignored.
	_ = os.WriteFile(filepath.Join(dir, ".x.json.tmp-123"), []byte("junk"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("junk"), 0o600)

	entries, err := s.List()
	if err != nil || len(entries) != 3 {
		t.Fatalf("List = %d entries, %v", len(entries), err)
	}
	got, err := s.Read(entries[0])
	if err != nil || len(got) != 2 || got[0].Sequence != 0 {
		t.Fatalf("oldest = %+v, %v", got, err)
	}
	info, _ := os.Stat(filepath.Join(dir, entries[0].Name))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", info.Mode().Perm())
	}
	if err := s.Remove(entries[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(entries[0]); err != nil {
		t.Fatalf("removing twice should be a no-op: %v", err)
	}
	if entries, _ = s.List(); len(entries) != 2 || entries[0].Name > entries[1].Name {
		t.Fatalf("after remove: %+v", entries)
	}
}

func TestPruneByAgeAndSize(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, _ := openAt(t, &now, time.Hour, 1<<20)
	for i := range 4 {
		if err := s.Put(samples(uint64(i))); err != nil {
			t.Fatal(err)
		}
		now = now.Add(30 * time.Minute)
	}
	// Now is 2h after the first file: files from 0m and 30m are older than an hour.
	dropped, err := s.Prune()
	if err != nil || dropped != 2 {
		t.Fatalf("age prune dropped %d, %v; want 2", dropped, err)
	}
	entries, _ := s.List()
	var size int64
	for _, e := range entries {
		size += e.Size
	}
	s.maxSize = size - 1
	if dropped, _ := s.Prune(); dropped != 1 {
		t.Fatalf("size prune dropped %d, want the single oldest file", dropped)
	}
	if entries, _ = s.List(); len(entries) != 1 {
		t.Fatalf("left %d files", len(entries))
	}
	if got, _ := s.Read(entries[0]); got[0].Sequence != 3 {
		t.Fatalf("kept sequence %d, want the newest (3)", got[0].Sequence)
	}
}

func TestReadCorruptFile(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s, dir := openAt(t, &now, time.Hour, 1<<20)
	_ = os.WriteFile(filepath.Join(dir, "00000000000000000001-00000000000000000000.json"), []byte("{"), 0o600)
	entries, _ := s.List()
	if _, err := s.Read(entries[0]); err == nil {
		t.Fatal("corrupt file: want error")
	}
}
