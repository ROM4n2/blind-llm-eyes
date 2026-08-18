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
	// Recorder is the optional tier lookup hook. When set, TwoTier itself
	// records tier=hot/cold events; the nested hot LRU and cold SQLite
	// Recorders are NOT touched by TwoTier — so callers who want granular
	// per-backend metrics can set hot.Recorder and cold.Recorder separately.
	Recorder TierRecorder

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
	// 1) hot layer. Avoid counting via t.hot's internal LRU recorder (which
	// would be tier="lru") by not setting it — instead we record tier="hot"
	// at the TwoTier level so operators see "hot" and "cold" as a unified
	// pair that sum to 100% of TwoTier lookups.
	t.hot.mu.Lock()
	if e, ok := t.hot.items[key]; ok {
		t.hot.ll.MoveToFront(e)
		v := e.Value.(*entry).value
		t.hot.mu.Unlock()
		if t.Recorder != nil {
			t.Recorder.OnLookup("hot", "hit")
		}
		return v, true
	}
	t.hot.mu.Unlock()
	if t.Recorder != nil {
		t.Recorder.OnLookup("hot", "miss")
	}

	// 2) cold layer + backfill (sharded lock to avoid herd without
	// serializing unrelated keys). Cold SQLite Get hook emits tier=cold via
	// its own Recorder if set, so we skip double-counting at this level.
	mu := t.shard(key)
	mu.Lock()
	defer mu.Unlock()
	// double-check: another goroutine may have just backfilled hot.
	t.hot.mu.Lock()
	if e, ok := t.hot.items[key]; ok {
		t.hot.ll.MoveToFront(e)
		v := e.Value.(*entry).value
		t.hot.mu.Unlock()
		// Don't emit another hot/hit — this is a re-check after hot miss was
		// already logged above; re-counting would make hot hits > cold misses.
		return v, true
	}
	t.hot.mu.Unlock()
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

// Close releases the cold-layer resources (the SQLite file handle). The hot
// LRU's Close is a no-op. Calling Close renders the TwoTier unusable for
// further Get/Put (cold queries will error); it is intended for the
// graceful-shutdown and config-reload swap paths.
func (t *TwoTier) Close() error { return t.cold.Close() }

// ColdRecorder returns the current cold SQLite Recorder. Used by the proxy
// metrics adapter to attach a SAME tier-recorder to the SQLite cold layer
// as the TwoTier hot layer, so operators get a unified hot+cold pair where
// cold events are also visible.
func (t *TwoTier) ColdRecorder() TierRecorder { return t.cold.Recorder }

// SetColdRecorder sets the cold SQLite Recorder. nil clears.
func (t *TwoTier) SetColdRecorder(r TierRecorder) { t.cold.Recorder = r }
