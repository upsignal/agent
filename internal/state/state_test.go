package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCredential(dir); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("empty dir: err = %v, want ErrNotEnrolled", err)
	}
	c := Credential{AgentID: "a1", MonitorID: "m1", Secret: "s3cr3t-value", BaseURL: "https://api.example.com",
		EnrolledAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}
	if err := SaveCredential(dir, c, KeepOwner); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, CredentialFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	got, err := LoadCredential(dir)
	if err != nil || got != c {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for _, s := range []string{c.String(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, c.Secret) {
			t.Errorf("formatted credential leaks the secret: %s", s)
		}
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("world-readable credential: err = %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt credential: err = %v", err)
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	for i := range 3 {
		if err := WriteFileAtomic(path, []byte{byte('a' + i)}, KeepOwner); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want only the target file", len(entries))
	}
	if data, _ := os.ReadFile(path); string(data) != "c" {
		t.Fatalf("content = %q", data)
	}
}

func TestSequenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSequence(dir)
	if err != nil {
		t.Fatal(err)
	}
	for want := range uint64(3) {
		if got, err := s.Next(); err != nil || got != want {
			t.Fatalf("Next = %d, %v; want %d", got, err, want)
		}
	}
	reopened, err := OpenSequence(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Next(); got != 3 {
		t.Fatalf("after restart Next = %d, want 3 (never reuse a sequence)", got)
	}
	if err := os.WriteFile(filepath.Join(dir, SequenceFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSequence(dir); err == nil {
		t.Fatal("corrupt sequence: want error, not a silent reset to 0")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	in := Status{UpdatedAt: now, LastUploadAt: &now, LastErrorCategory: "network", SpooledFiles: 2}
	if err := WriteStatus(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadStatus(dir)
	if err != nil || !out.LastUploadAt.Equal(now) || out.SpooledFiles != 2 || out.LastErrorCategory != "network" {
		t.Fatalf("status = %+v, %v", out, err)
	}
}
