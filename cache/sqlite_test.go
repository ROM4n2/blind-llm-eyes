package cache

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestOpenSQLite_CreatesTableAndIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	var name string
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='cache'").Scan(&name)
	if err != nil || name != "cache" {
		t.Fatalf("table cache missing: name=%q err=%v", name, err)
	}
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_cache_last_accessed'").Scan(&name)
	if err != nil || name != "idx_cache_last_accessed" {
		t.Fatalf("index missing: name=%q err=%v", name, err)
	}
}

func TestOpenSQLite_DefaultPath(t *testing.T) {
	// Empty path -> ./cache.db in cwd. Use t.Chdir to isolate (Go 1.24+).
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := OpenSQLite("", 0, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite empty path: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat("./cache.db"); err != nil {
		t.Fatalf("default db file not created: %v", err)
	}
}

func TestOpenSQLite_IdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s1, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s1.Close()
	// Reopening the same path should not error and should keep the schema.
	s2, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	var n int
	_ = s2.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='cache'").Scan(&n)
	if n != 1 {
		t.Fatalf("schema not preserved on reopen: n=%d", n)
	}
}
