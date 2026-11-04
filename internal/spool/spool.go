// Package spool buffers samples on disk while the API is unreachable.
//
// Each file holds the samples of one failed batch as JSON, named <unix-nanos>-<first-sequence>.json so
// lexical order is oldest first. Files are written atomically with mode 0600 in a 0700 directory, and
// never contain credentials. The spool is bounded by age and total size; the oldest files go first.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/upsignal/agent/internal/state"
	"github.com/upsignal/agent/internal/telemetry"
)

type Spool struct {
	dir     string
	maxAge  time.Duration
	maxSize int64
	now     func() time.Time
}

type Entry struct {
	Name    string
	Size    int64
	Created time.Time
}

func Open(dir string, maxAge time.Duration, maxSize int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Spool{dir: dir, maxAge: maxAge, maxSize: maxSize, now: time.Now}, nil
}

func (s *Spool) Put(samples []telemetry.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	data, err := json.Marshal(samples)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%020d.json", s.now().UnixNano(), samples[0].Sequence)
	return state.WriteFileAtomic(filepath.Join(s.dir, name), data, state.KeepOwner)
}

// List returns spooled files, oldest first. Temporary and foreign files are ignored.
func (s *Spool) List() ([]Entry, error) {
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range dirents {
		created, ok := parseName(d.Name())
		if !ok || !d.Type().IsRegular() {
			continue
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Name: d.Name(), Size: info.Size(), Created: created})
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func parseName(name string) (time.Time, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || strings.HasPrefix(name, ".") {
		return time.Time{}, false
	}
	nanos, _, ok := strings.Cut(base, "-")
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

func (s *Spool) Read(e Entry) ([]telemetry.Sample, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, e.Name))
	if err != nil {
		return nil, err
	}
	var samples []telemetry.Sample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil, fmt.Errorf("spool file %s is corrupt: %w", e.Name, err)
	}
	return samples, nil
}

func (s *Spool) Remove(e Entry) error {
	err := os.Remove(filepath.Join(s.dir, e.Name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Prune enforces the age and size limits, dropping the oldest files first. It returns how many files
// it dropped so the caller can warn.
func (s *Spool) Prune() (int, error) {
	entries, err := s.List()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	cutoff := s.now().Add(-s.maxAge)
	dropped := 0
	for _, e := range entries {
		if !e.Created.Before(cutoff) && total <= s.maxSize {
			break
		}
		if err := s.Remove(e); err != nil {
			return dropped, err
		}
		total -= e.Size
		dropped++
	}
	return dropped, nil
}
