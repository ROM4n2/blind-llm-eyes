package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadPidfile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "pidfile.json")
	original := PidfileData{
		PID:       12345,
		Addr:      "127.0.0.1:8790",
		Token:     "deadbeef",
		StartedAt: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
	}
	if err := WritePidfile(path, original); err != nil {
		t.Fatalf("write: %v", err)
	}
	// WritePidfile must create parent dirs.
	got, err := ReadPidfile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.PID != original.PID || got.Addr != original.Addr || got.Token != original.Token {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, original)
	}
	if !got.StartedAt.Equal(original.StartedAt) {
		t.Fatalf("started_at mismatch: got %v, want %v", got.StartedAt, original.StartedAt)
	}
}

func TestReadPidfile_NotExist(t *testing.T) {
	_, err := ReadPidfile(filepath.Join(t.TempDir(), "nope.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist error, got %v", err)
	}
}

func TestWritePidfile_AtomicReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pidfile.json")
	if err := WritePidfile(path, PidfileData{PID: 1, Addr: "a", Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := WritePidfile(path, PidfileData{PID: 2, Addr: "b", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPidfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 2 || got.Addr != "b" || got.Token != "t2" {
		t.Fatalf("expected updated data, got %+v", got)
	}
}
