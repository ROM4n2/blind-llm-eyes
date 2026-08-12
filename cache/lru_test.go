package cache

import "testing"

func TestLRU_GetPut(t *testing.T) {
	c := NewLRU(2) // 容量 2
	c.Put("k1", "desc1")
	if v, ok := c.Get("k1"); !ok || v != "desc1" {
		t.Fatalf("k1 miss: ok=%v v=%q", ok, v)
	}

	// 塞满容量
	c.Put("k2", "desc2")
	c.Put("k3", "desc3") // 应该踢掉 k1

	if _, ok := c.Get("k1"); ok {
		t.Errorf("k1 should be evicted")
	}
	if v, ok := c.Get("k2"); !ok || v != "desc2" {
		t.Errorf("k2 wrong: ok=%v v=%q", ok, v)
	}
}

func TestLRU_GetPromotes(t *testing.T) {
	c := NewLRU(2)
	c.Put("a", "1")
	c.Put("b", "2")
	_, _ = c.Get("a") // 提升 a 为最近使用
	c.Put("c", "3")   // 应踢掉 b，不是 a
	if _, ok := c.Get("b"); ok {
		t.Errorf("b should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Errorf("a should survive")
	}
}
