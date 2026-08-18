package cache

import (
	"container/list"
	"sync"
)

// LRU 是线程安全的 hash→描述 缓存。零值不可用，用 NewLRU。
type LRU struct {
	mu       sync.Mutex
	cap      int
	ll       *list.List // 最近用的在 front
	items    map[string]*list.Element
	Recorder TierRecorder // 可选指标回调；nil = 不埋点
}

type entry struct {
	key   string
	value string // description
}

func NewLRU(capacity int) *LRU {
	return &LRU{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}
}

func (c *LRU) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		if c.Recorder != nil {
			c.Recorder.OnLookup("lru", "hit")
		}
		return e.Value.(*entry).value, true
	}
	if c.Recorder != nil {
		c.Recorder.OnLookup("lru", "miss")
	}
	return "", false
}

func (c *LRU) Put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		e.Value.(*entry).value = value
		return
	}
	e := c.ll.PushFront(&entry{key: key, value: value})
	c.items[key] = e
	for c.ll.Len() > c.cap {
		c.removeOldest()
	}
}

// removeOldest 必须在锁内调用
func (c *LRU) removeOldest() {
	e := c.ll.Back()
	if e == nil {
		return
	}
	c.ll.Remove(e)
	ent := e.Value.(*entry)
	delete(c.items, ent.key)
}

// Close is a no-op for the in-memory LRU: there are no backing resources
// to release. It exists to satisfy the Cache interface so LRU and TwoTier
// can be used interchangeably in shutdown / reload paths.
func (c *LRU) Close() error { return nil }
