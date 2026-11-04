// Package state persists the agent's local files in the state directory (/var/lib/upsignal-agent):
// the credential, the sample sequence counter, and a status summary for `upsignal-agent status`.
// Every write is atomic (temp file, fsync, rename) so a crash never leaves a torn file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CredentialFile = "agent-credential"
	SequenceFile   = "sequence"
	StatusFile     = "status.json"
)

var ErrNotEnrolled = errors.New("not enrolled: run `sudo upsignal-agent enroll`")

// Owner is the uid/gid files are chowned to; -1 leaves ownership unchanged.
type Owner struct{ UID, GID int }

var KeepOwner = Owner{-1, -1}

// WriteFileAtomic replaces path with data, readable only by its owner.
func WriteFileAtomic(path string, data []byte, owner Owner) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if owner.UID >= 0 {
		if err := tmp.Chown(owner.UID, owner.GID); err != nil {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Make the rename itself durable.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Credential is what enrollment returns. The secret is never logged or printed.
type Credential struct {
	AgentID    string    `json:"agentId"`
	MonitorID  string    `json:"monitorId"`
	Secret     string    `json:"credential"`
	BaseURL    string    `json:"baseUrl"`
	EnrolledAt time.Time `json:"enrolledAt"`
}

// String and GoString keep the secret out of logs and %v / %#v output.
func (c Credential) String() string {
	return fmt.Sprintf("agent %s (server %s)", c.AgentID, c.MonitorID)
}
func (c Credential) GoString() string { return c.String() }

func LoadCredential(dir string) (Credential, error) {
	path := filepath.Join(dir, CredentialFile)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Credential{}, ErrNotEnrolled
	}
	if err != nil {
		return Credential{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credential{}, fmt.Errorf("%s is readable by other users (mode %04o); expected 0600",
			path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, err
	}
	var c Credential
	if err := json.Unmarshal(data, &c); err != nil || c.Secret == "" || c.AgentID == "" {
		return Credential{}, fmt.Errorf("%s is corrupt; enroll again with --force", path)
	}
	return c, nil
}

func SaveCredential(dir string, c Credential, owner Owner) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, CredentialFile), data, owner)
}

// Sequence hands out sample sequence numbers that are never reused, even across restarts.
// The next value is persisted before the current one is returned.
type Sequence struct {
	mu   sync.Mutex
	path string
	next uint64
}

func OpenSequence(dir string) (*Sequence, error) {
	path := filepath.Join(dir, SequenceFile)
	s := &Sequence{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", path, err)
	}
	s.next = n
	return s, nil
}

func (s *Sequence) Next() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := WriteFileAtomic(s.path, []byte(strconv.FormatUint(s.next+1, 10)), KeepOwner); err != nil {
		return 0, fmt.Errorf("persist sequence: %w", err)
	}
	v := s.next
	s.next++
	return v, nil
}

// Status is a summary written by the running agent for `upsignal-agent status`.
type Status struct {
	UpdatedAt         time.Time  `json:"updatedAt"`
	LastUploadAt      *time.Time `json:"lastUploadAt"`
	LastErrorAt       *time.Time `json:"lastErrorAt"`
	LastErrorCategory string     `json:"lastErrorCategory,omitempty"`
	SpooledFiles      int        `json:"spooledFiles"`
	Revoked           bool       `json:"revoked,omitempty"`
}

func WriteStatus(dir string, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, StatusFile), data, KeepOwner)
}

func ReadStatus(dir string) (Status, error) {
	var s Status
	data, err := os.ReadFile(filepath.Join(dir, StatusFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}
