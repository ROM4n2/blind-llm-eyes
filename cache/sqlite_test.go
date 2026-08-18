package cache

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

func TestSQLite_PutUpdatesInMemoryCount(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	if got := s.count.Load(); got != 0 {
		t.Fatalf("initial count = %d, want 0", got)
	}
	s.Put("h1", "desc1")
	s.Put("h2", "desc2")
	s.Put("h3", "desc3")
	if got := s.count.Load(); got != 3 {
		t.Fatalf("after 3 inserts count = %d, want 3", got)
	}
	// Update existing key: counter must not change.
	s.Put("h1", "desc1-updated")
	if got := s.count.Load(); got != 3 {
		t.Fatalf("after update count = %d, want 3", got)
	}
	// Counter must agree with the DB row count.
	var dbN int
	if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
		t.Fatal(err)
	}
	if int64(dbN) != s.count.Load() {
		t.Fatalf("counter %d != db rows %d", s.count.Load(), dbN)
	}
}

func TestSQLite_EvictDecrementsCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := OpenSQLite(path, 5, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		s.Put(fmt.Sprintf("h%d", i), "desc")
	}
	if got := s.count.Load(); got > 5 {
		t.Fatalf("after evict count = %d, want <= 5", got)
	}
	var dbN int
	if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
		t.Fatal(err)
	}
	if int64(dbN) != s.count.Load() {
		t.Fatalf("counter %d != db rows %d after evict", s.count.Load(), dbN)
	}
}

func TestSQLite_RebuildResetsCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := OpenSQLite(path, 100, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	s.Put("h1", "desc1")
	s.Put("h2", "desc2")
	if got := s.count.Load(); got != 2 {
		t.Fatalf("pre-rebuild count = %d, want 2", got)
	}
	if err := s.rebuildDB(path); err != nil {
		t.Fatalf("rebuildDB: %v", err)
	}
	if err := s.initSchema(); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if err := s.initCount(); err != nil {
		t.Fatalf("initCount: %v", err)
	}
	if got := s.count.Load(); got != 0 {
		t.Fatalf("post-rebuild count = %d, want 0", got)
	}
}

// TestSQLite_EvictNoThunderingHerd verifies the CAS guard prevents the
// "evict storm" where bursty concurrent Puts each trigger DELETE and clear
// the cache far below the configured cap.
//
// Without the guard: 50 goroutines crossing maxEntries=5 simultaneously
// would each run DELETE LIMIT 41, eventually emptying the cache.
//
// With the guard: at most one evict runs at a time; concurrent losers return
// early. count may transiently exceed maxEntries during the burst window but
// converges on the next Put.
func TestSQLite_EvictNoThunderingHerd(t *testing.T) {
	maxEntries := 5
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := OpenSQLite(path, maxEntries, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	// 50 goroutines released by a barrier to maximize concurrent Put
	// arrival — this is the burst that triggers the storm without the CAS.
	const N = 50
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			<-barrier
			s.Put(fmt.Sprintf("k%d", i), "v")
		}(i)
	}
	close(barrier)
	wg.Wait()

	// Assertion 1: cache was not emptied by a concurrent evict storm.
	if s.count.Load() == 0 {
		t.Fatal("cache was emptied by concurrent evict storm (CAS guard missing or ineffective)")
	}

	// Assertion 2: counter agrees with the actual DB row count (no drift).
	var dbN int
	if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
		t.Fatal(err)
	}
	if int64(dbN) != s.count.Load() {
		t.Fatalf("counter %d != db rows %d after concurrent puts", s.count.Load(), dbN)
	}

	// Assertion 3: CAS lets evict converge once the burst subsides. We
	// manually trigger a few evictIfNeeded calls (the next Put would also
	// do this, but we want a deterministic check) and expect count to fall
	// back to <= maxEntries.
	for i := 0; i < 5; i++ {
		s.evictIfNeeded()
	}
	if s.count.Load() > int64(maxEntries) {
		t.Fatalf("after manual evicts, counter %d > maxEntries %d (CAS did not converge)", s.count.Load(), maxEntries)
	}

	// Assertion 4: counter still agrees with the DB after convergence.
	if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
		t.Fatal(err)
	}
	if int64(dbN) != s.count.Load() {
		t.Fatalf("counter %d != db rows %d after convergence", s.count.Load(), dbN)
	}
}

// TestSQLite_ActualCountAndMemoryCount verifies the exported observability
// methods used by the CLI `cache stats` command for drift detection.
// MemoryCount reads the in-memory atomic counter; ActualCount queries the
// DB. After a sequence of Put operations they must agree. After a manual
// DELETE (simulating an external writer or counter drift), they diverge.
func TestSQLite_ActualCountAndMemoryCount(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()

	// Initial state: 0 entries, memory and actual agree.
	if got := s.MemoryCount(); got != 0 {
		t.Fatalf("initial MemoryCount = %d, want 0", got)
	}
	actual, err := s.ActualCount()
	if err != nil {
		t.Fatalf("ActualCount: %v", err)
	}
	if actual != 0 {
		t.Fatalf("initial ActualCount = %d, want 0", actual)
	}

	// Insert 3 entries; memory counter is maintained incrementally.
	s.Put("h1", "v1")
	s.Put("h2", "v2")
	s.Put("h3", "v3")
	if got := s.MemoryCount(); got != 3 {
		t.Fatalf("after 3 puts MemoryCount = %d, want 3", got)
	}
	actual, err = s.ActualCount()
	if err != nil {
		t.Fatalf("ActualCount: %v", err)
	}
	if actual != 3 {
		t.Fatalf("after 3 puts ActualCount = %d, want 3", actual)
	}

	// Simulate drift: external DELETE that bypasses the counter.
	if _, err := s.db.Exec("DELETE FROM cache WHERE hash = ?", "h1"); err != nil {
		t.Fatalf("external delete: %v", err)
	}
	// MemoryCount still reflects the pre-delete state (drift!).
	if got := s.MemoryCount(); got != 3 {
		t.Fatalf("after external delete MemoryCount = %d, want 3 (drift)", got)
	}
	// ActualCount reflects the true DB state.
	actual, err = s.ActualCount()
	if err != nil {
		t.Fatalf("ActualCount: %v", err)
	}
	if actual != 2 {
		t.Fatalf("after external delete ActualCount = %d, want 2", actual)
	}
}
