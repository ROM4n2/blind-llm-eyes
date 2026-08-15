package cache

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestOpenSQLite_CorruptionRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	// Write garbage bytes to simulate a corrupted DB file.
	if err := os.WriteFile(path, []byte("not a sqlite database garbage garbage garbage"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	// OpenSQLite should detect corruption and rebuild (not return an error).
	s, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite on corrupt db: %v", err)
	}
	defer s.Close()
	// After recovery the DB should be usable.
	s.Put("h1", "v")
	if got, _ := s.Get("h1"); got != "v" {
		t.Fatalf("after recovery Get=%q want v", got)
	}
}

func newTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

func TestSQLite_PutGetRoundTrip(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	s.Put("h1", "a cat sitting on a mat")
	got, ok := s.Get("h1")
	if !ok || got != "a cat sitting on a mat" {
		t.Fatalf("got (%q,%v), want (\"a cat...\", true)", got, ok)
	}
}

func TestSQLite_GetMiss(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestSQLite_UpsertOverwrites(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	s.Put("h1", "v1")
	s.Put("h1", "v2")
	got, _ := s.Get("h1")
	if got != "v2" {
		t.Fatalf("want v2, got %q", got)
	}
}

func TestSQLite_TouchAccessOnGet(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	s.Put("h1", "v")
	// Force last_accessed to 0 (ancient).
	_, _ = s.db.Exec("UPDATE cache SET last_accessed = 0 WHERE hash = ?", "h1")
	s.Get("h1")
	var la int64
	_ = s.db.QueryRow("SELECT last_accessed FROM cache WHERE hash = ?", "h1").Scan(&la)
	if la == 0 {
		t.Fatal("Get did not update last_accessed")
	}
}

func TestSQLite_EvictByCount(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 3, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	for i := 0; i < 5; i++ {
		s.Put(fmt.Sprintf("h%d", i), "v")
		time.Sleep(2 * time.Millisecond) // stagger last_accessed
	}
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM cache").Scan(&n)
	if n > 3 {
		t.Fatalf("count %d > maxEntries 3", n)
	}
}

func TestSQLite_EvictByTTL(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 0, 1*time.Millisecond, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	s.Put("h1", "v")
	time.Sleep(20 * time.Millisecond)
	s.Put("h2", "v") // triggers TTL eviction of h1
	if _, ok := s.Get("h1"); ok {
		t.Fatal("h1 should have been TTL-evicted")
	}
}
