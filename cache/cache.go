package cache

// Cache is the hash→description cache abstraction.
// *LRU (in-memory) and *TwoTier (LRU+SQLite) both implement it.
// handler depends only on this interface so different backends / mocks
// can be injected.
//
// Close releases backend resources (e.g. the SQLite file handle). It is
// idempotent and safe to call from main.go's graceful-shutdown path as well
// as from the reload side-effect chain (Task 9) when swapping to a new cache
// instance after a config reload.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, value string)
	Close() error
}

// TierRecorder is the optional observability hook for cache implementations.
// On each Get, implementations call OnLookup(tier, result) so a single
// counter (metrics.CacheHitsTotal) can be partitioned by tier + outcome
// without making the cache package directly depend on the metrics package
// (which would cause a circular import through proxy).
//
// Zero-value (nil) = no recording; hot-path overhead is a single nil-check.
type TierRecorder interface {
	// OnLookup fires exactly once per logical tier access.
	// tier ∈ {"hot", "cold", "lru"}; result ∈ {"hit", "miss"}.
	OnLookup(tier, result string)
}

// Compile-time assertions that concrete types satisfy the Cache interface.
var (
	_ Cache = (*LRU)(nil)
	_ Cache = (*TwoTier)(nil)
)
