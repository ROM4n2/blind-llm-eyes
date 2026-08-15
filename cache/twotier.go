package cache

import (
	"log/slog"
	"sync"
)

// TwoTier is a composite cache: LRU (hot, in-memory) + SQLite (cold, persistent).
//   Get: check LRU first; on miss, query SQLite and backfill LRU.
//   Put: write to both layers (SQLite uses WAL, so the write is fast).
// Eviction is split: LRU eviction only drops the in-memory copy (the entry
// stays in SQLite and can be reloaded on the next Get); SQLite eviction is
// the real deletion (see SQLite.evictIfNeeded).
type TwoTier struct {
	hot  *LRU
	cold *SQLite
	log  *slog.Logger

	// mu serializes the Get "query cold → backfill hot" step to prevent a
	// thundering herd of concurrent waiters all backfilling the same key.
	// Put does NOT take this lock: LRU and SQLite are each independently
	// thread-safe, and duplicate writes are idempotent overwrites.
	mu sync.Mutex
}

// NewTwoTier constructs a TwoTier cache. lruCap is the hot-layer capacity.
// A nil logger defaults to slog.Default().
func NewTwoTier(lruCap int, cold *SQLite, logger *slog.Logger) *TwoTier {
	if logger == nil {
		logger = slog.Default()
	}
	return &TwoTier{hot: NewLRU(lruCap), cold: cold, log: logger}
}

func (t *TwoTier) Get(key string) (string, bool) {
	// 1) hot layer
	if v, ok := t.hot.Get(key); ok {
		return v, true
	}
	// 2) cold layer + backfill (serialized to avoid herd)
	t.mu.Lock()
	defer t.mu.Unlock()
	// double-check: another goroutine may have just backfilled hot.
	if v, ok := t.hot.Get(key); ok {
		return v, true
	}
	desc, ok := t.cold.Get(key)
	if !ok {
		return "", false
	}
	t.hot.Put(key, desc)
	return desc, true
}

func (t *TwoTier) Put(key, value string) {
	t.hot.Put(key, value)
	t.cold.Put(key, value) // best-effort; Put logs internally on error
}
