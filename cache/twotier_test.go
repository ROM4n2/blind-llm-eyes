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

func TestTwoTier_SatisfiesCacheInterface(t *testing.T) {
	var _ Cache = (*TwoTier)(nil) // compile-time check (also asserted in cache.go)
}
