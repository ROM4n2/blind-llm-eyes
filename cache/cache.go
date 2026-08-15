package cache

// Cache is the hash→description cache abstraction.
// *LRU (in-memory) and *TwoTier (LRU+SQLite) both implement it.
// handler depends only on this interface so different backends / mocks
// can be injected.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, value string)
}

// Compile-time assertions that concrete types satisfy the Cache interface.
var (
	_ Cache = (*LRU)(nil)
	_ Cache = (*TwoTier)(nil)
)
