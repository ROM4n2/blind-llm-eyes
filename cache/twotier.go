package cache

import (
	"log/slog"
	"sync"
)

const shardCount = 16

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

	// shards serialize the Get "query cold → backfill hot" step per-key to
	// prevent a thundering herd of concurrent waiters all backfilling the same
	// key. Different keys hash to different shards, so cross-key queries run
	// in parallel. Put does NOT take any lock: LRU and SQLite are each
	// independently thread-safe, and duplicate writes are idempotent overwrites.
	shards [shardCount]sync.Mutex
}

// NewTwoTier constructs a TwoTier cache. lruCap is the hot-layer capacity.
// A nil logger defaults to slog.Default().
func NewTwoTier(lruCap int, cold *SQLite, logger *slog.Logger) *TwoTier {
	if logger == nil {
		logger = slog.Default()
	}
	return &TwoTier{hot: NewLRU(lruCap), cold: cold, log: logger}
}

// shard returns the mutex for the given key, using FNV-32a hash for
// even distribution across shards. The hash is computed inline (no
// allocation) rather than via hash/fnv's New32a() which allocates a hash.Hash
// interface box per call on the hot path.
func (t *TwoTier) shard(key string) *sync.Mutex {
	// FNV-32a: https://datatracker.ietf.org/doc/html/draft-eastlake-fnv
	const (
		offsetBasis uint32 = 2166136261
		prime      uint32 = 16777619
	)
	h := offsetBasis
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= prime
	}
	return &t.shards[h%shardCount]
}

func (t *TwoTier) Get(key string) (string, bool) {
	// 1) hot layer
	if v, ok := t.hot.Get(key); ok {
		return v, true
	}
	// 2) cold layer + backfill (sharded lock to avoid herd without
	// serializing unrelated keys)
	mu := t.shard(key)
	mu.Lock()
	defer mu.Unlock()
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
