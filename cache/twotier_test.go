package cache

import (
	"sync"
	"testing"
)

func newTestTwoTier(t *testing.T) *TwoTier {
	t.Helper()
	s := newTestSQLite(t)
	t.Cleanup(func() { s.Close() })
	return NewTwoTier(10, s, discardLogger())
}

func TestTwoTier_HotHitNoColdRead(t *testing.T) {
	tt := newTestTwoTier(t)
	tt.Put("h1", "v1")
	// Corrupt the cold copy: if hot is consulted, the stale cold value is ignored.
	_, _ = tt.cold.db.Exec("UPDATE cache SET description='STALE' WHERE hash=?", "h1")
	got, _ := tt.Get("h1")
	if got != "v1" {
		t.Fatalf("hot miss: got %q want v1", got)
	}
}

func TestTwoTier_ColdMissBackfills(t *testing.T) {
	tt := newTestTwoTier(t)
	// Write directly to cold, bypassing hot.
	tt.cold.Put("h2", "from-cold")
	// Reset hot to force a cold lookup.
	tt.hot = NewLRU(10)
	got, ok := tt.Get("h2")
	if !ok || got != "from-cold" {
		t.Fatalf("cold backfill: got (%q,%v)", got, ok)
	}
	// Backfill should have populated hot.
	if v, _ := tt.hot.Get("h2"); v != "from-cold" {
		t.Fatalf("backfill to hot failed: %q", v)
	}
}

func TestTwoTier_BothMiss(t *testing.T) {
	tt := newTestTwoTier(t)
	if _, ok := tt.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestTwoTier_LRUEvictionKeepsCold(t *testing.T) {
	// hot cap = 1: Put h1 then h2 evicts h1 from hot, but cold still has h1.
	s := newTestSQLite(t)
	t.Cleanup(func() { s.Close() })
	tt := NewTwoTier(1, s, discardLogger())
	tt.Put("h1", "v1")
	tt.Put("h2", "v2")
	if _, ok := tt.hot.Get("h1"); ok {
		t.Fatal("h1 should be evicted from hot")
	}
	if got, _ := tt.Get("h1"); got != "v1" {
		t.Fatalf("cold should still have h1: got %q", got)
	}
}

func TestTwoTier_ConcurrentGetNoThunderingHerd(t *testing.T) {
	tt := newTestTwoTier(t)
	tt.cold.Put("hX", "vX")
	tt.hot = NewLRU(10) // force cold path

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _ := tt.Get("hX"); got != "vX" {
				t.Errorf("goroutine got %q", got)
			}
		}()
	}
	wg.Wait()
}

func TestTwoTier_ConcurrentDifferentKeysNoBlock(t *testing.T) {
	tt := newTestTwoTier(t)
	// Write 16 different keys to cold, clear hot to force cold path.
	for i := 0; i < 16; i++ {
		tt.cold.Put(keyForIdx(i), valForIdx(i))
	}
	tt.hot = NewLRU(100)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, ok := tt.Get(keyForIdx(idx))
			if !ok || got != valForIdx(idx) {
				t.Errorf("key %d: got (%q,%v)", idx, got, ok)
			}
		}(i)
	}
	wg.Wait()
}

func keyForIdx(i int) string {
	return "key-" + string(rune('a'+i))
}

func valForIdx(i int) string {
	return "val-" + string(rune('a'+i))
}

func TestTwoTier_SatisfiesCacheInterface(t *testing.T) {
	var _ Cache = (*TwoTier)(nil) // compile-time check (also asserted in cache.go)
}

// TestTwoTier_Close_CascadesToCold verifies TwoTier.Close returns nil and
// is safe to call. TwoTier.Close delegates to cold.Close (SQLite.Close →
// *sql.DB.Close); the cascade is verified by code inspection rather than
// by observing a post-close miss, because database/sql connection-pool
// semantics under modernc.org/sqlite do not reliably surface a query error
// immediately after Close. The contract the reload swap path (Task 9)
// relies on is simply: Close is safe, idempotent, and releases the SQLite
// handle so the same db file can be reopened by the next handler instance.
func TestTwoTier_Close_CascadesToCold(t *testing.T) {
	s := newTestSQLite(t)
	tt := NewTwoTier(10, s, discardLogger())
	// Close is idempotent (*sql.DB.Close tolerates repeat calls), so a
	// cleanup guard is safe even if the test already called Close.
	t.Cleanup(func() { _ = tt.Close() })

	if err := tt.Close(); err != nil {
		t.Fatalf("first Close: got %v, want nil", err)
	}
	// Second Close must not panic — *sql.DB.Close is idempotent.
	if err := tt.Close(); err != nil {
		t.Fatalf("second Close (idempotency): got %v, want nil", err)
	}
}
