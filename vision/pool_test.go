package vision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/logging"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
)

// mockProvider 是测试用的 VisionProvider 实现，可控制返回值和调用计数。
type mockProvider struct {
	name      string
	callCount atomic.Int64
	result    string
	err       error
	delay     time.Duration
}

func (m *mockProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	m.callCount.Add(1)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if m.err != nil {
		return "", m.err
	}
	return m.result, nil
}

func newMock(name, result string, err error) *mockProvider {
	return &mockProvider{name: name, result: result, err: err}
}

func TestPool_PriorityOrdering(t *testing.T) {
	// priority 2 的 provider 放在 entries[0]，priority 1 的放在 entries[1]
	// 排序后 priority 1 应该先被调用
	p1 := newMock("high-priority", "from-high", nil)
	p2 := newMock("low-priority", "from-low", nil)

	entries := []PoolEntry{
		{Name: "low-priority", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "high-priority", Provider: p1, Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, err := NewPool(entries, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	desc, err := pool.DescribeImage(context.Background(), "data", "image/png", 100)
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if desc != "from-high" {
		t.Errorf("want %q, got %q", "from-high", desc)
	}
	if p1.callCount.Load() != 1 {
		t.Errorf("high-priority call count: want 1, got %d", p1.callCount.Load())
	}
	if p2.callCount.Load() != 0 {
		t.Errorf("low-priority should not be called, got %d", p2.callCount.Load())
	}
}

func TestPool_FailoverOnError(t *testing.T) {
	p1 := newMock("primary", "", errors.New("mimo down"))
	p2 := newMock("fallback", "fallback-desc", nil)

	entries := []PoolEntry{
		{Name: "primary", Provider: p1, Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "fallback", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, _ := NewPool(entries, slog.Default(), nil)

	desc, err := pool.DescribeImage(context.Background(), "data", "image/png", 100)
	if err != nil {
		t.Fatalf("want success via failover, got err: %v", err)
	}
	if desc != "fallback-desc" {
		t.Errorf("want %q, got %q", "fallback-desc", desc)
	}
	if p1.callCount.Load() != 1 {
		t.Errorf("primary call count: want 1, got %d", p1.callCount.Load())
	}
	if p2.callCount.Load() != 1 {
		t.Errorf("fallback call count: want 1, got %d", p2.callCount.Load())
	}
}

func TestPool_SkipOpenCircuitBreaker(t *testing.T) {
	p1 := newMock("primary", "should-not-be-called", nil)
	p2 := newMock("fallback", "fallback-desc", nil)

	cb1 := NewCircuitBreaker(2, 30*time.Second)
	// 开启 primary 的熔断器
	cb1.Allow()
	cb1.RecordFailure()
	cb1.Allow()
	cb1.RecordFailure()
	if cb1.State() != CBOpen {
		t.Fatalf("want primary CB open, got %s", cb1.State())
	}

	entries := []PoolEntry{
		{Name: "primary", Provider: p1, Priority: 1, CB: cb1},
		{Name: "fallback", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, _ := NewPool(entries, slog.Default(), nil)

	desc, err := pool.DescribeImage(context.Background(), "data", "image/png", 100)
	if err != nil {
		t.Fatalf("want success via fallback, got err: %v", err)
	}
	if desc != "fallback-desc" {
		t.Errorf("want %q, got %q", "fallback-desc", desc)
	}
	if p1.callCount.Load() != 0 {
		t.Errorf("primary should be skipped (CB open), got %d calls", p1.callCount.Load())
	}
	if p2.callCount.Load() != 1 {
		t.Errorf("fallback call count: want 1, got %d", p2.callCount.Load())
	}
}

func TestPool_AllFailReturnsError(t *testing.T) {
	p1 := newMock("primary", "", errors.New("err1"))
	p2 := newMock("fallback", "", errors.New("err2"))

	entries := []PoolEntry{
		{Name: "primary", Provider: p1, Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "fallback", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, _ := NewPool(entries, slog.Default(), nil)

	_, err := pool.DescribeImage(context.Background(), "data", "image/png", 100)
	if err == nil {
		t.Fatal("want error when all providers fail")
	}
	if !strings.Contains(err.Error(), "all 2 providers failed") {
		t.Errorf("error message should mention all providers failed, got: %v", err)
	}
	if p1.callCount.Load() != 1 || p2.callCount.Load() != 1 {
		t.Errorf("both providers should be tried once, got p1=%d p2=%d",
			p1.callCount.Load(), p2.callCount.Load())
	}
}

func TestPool_SuccessDoesNotFailover(t *testing.T) {
	p1 := newMock("primary", "primary-desc", nil)
	p2 := newMock("fallback", "should-not-be-called", nil)

	entries := []PoolEntry{
		{Name: "primary", Provider: p1, Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "fallback", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, _ := NewPool(entries, slog.Default(), nil)

	desc, err := pool.DescribeImage(context.Background(), "data", "image/png", 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if desc != "primary-desc" {
		t.Errorf("want %q, got %q", "primary-desc", desc)
	}
	if p2.callCount.Load() != 0 {
		t.Errorf("fallback should not be called on primary success, got %d", p2.callCount.Load())
	}
}

func TestPool_CircuitBreakerOpensAfterThreshold(t *testing.T) {
	p1 := newMock("primary", "", errors.New("always fails"))
	p2 := newMock("fallback", "fallback-desc", nil)

	// threshold=3：3 次失败后 primary 熔断
	entries := []PoolEntry{
		{Name: "primary", Provider: p1, Priority: 1, CB: NewCircuitBreaker(3, 30*time.Second)},
		{Name: "fallback", Provider: p2, Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}

	pool, _ := NewPool(entries, slog.Default(), nil)

	ctx := context.Background()

	// 前 3 次请求：primary 尝试失败，fallback 成功
	for i := 0; i < 3; i++ {
		desc, err := pool.DescribeImage(ctx, "data", "image/png", 100)
		if err != nil {
			t.Fatalf("request %d: unexpected err %v", i, err)
		}
		if desc != "fallback-desc" {
			t.Errorf("request %d: want fallback desc, got %q", i, desc)
		}
	}

	// primary 应被调用了 3 次（每次都失败）
	if p1.callCount.Load() != 3 {
		t.Errorf("primary call count: want 3, got %d", p1.callCount.Load())
	}

	// primary 熔断器应已开启
	if entries[0].CB.State() != CBOpen {
		t.Errorf("primary CB should be open after 3 failures, got %s", entries[0].CB.State())
	}

	// 第 4 次请求：primary 被跳过（熔断器开启），fallback 直接成功
	desc, err := pool.DescribeImage(ctx, "data", "image/png", 100)
	if err != nil {
		t.Fatalf("request 4: unexpected err %v", err)
	}
	if desc != "fallback-desc" {
		t.Errorf("request 4: want fallback desc, got %q", desc)
	}

	// primary 调用计数应仍为 3（第 4 次被跳过）
	if p1.callCount.Load() != 3 {
		t.Errorf("primary call count after CB open: want 3, got %d", p1.callCount.Load())
	}
}

func TestPool_RequestIDPropagated(t *testing.T) {
	p1 := &reqIDCheckingProvider{result: "ok"}
	entries := []PoolEntry{
		{Name: "p1", Provider: p1, Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
	}
	pool, _ := NewPool(entries, slog.Default(), nil)

	ctx := logging.WithRequestID(context.Background(), "test-req-123")
	_, err := pool.DescribeImage(ctx, "data", "image/png", 100)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p1.gotRequestID != "test-req-123" {
		t.Errorf("request_id not propagated: want %q, got %q", "test-req-123", p1.gotRequestID)
	}
}

// reqIDCheckingProvider 检查 context 中的 request_id 是否正确传播。
type reqIDCheckingProvider struct {
	gotRequestID string
	result       string
}

func (p *reqIDCheckingProvider) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	p.gotRequestID = logging.GetRequestID(ctx)
	return p.result, nil
}

func TestPool_NewPoolValidation(t *testing.T) {
	t.Run("empty entries", func(t *testing.T) {
		_, err := NewPool(nil, slog.Default(), nil)
		if err == nil {
			t.Error("want error for empty entries")
		}
	})

	t.Run("nil provider", func(t *testing.T) {
		_, err := NewPool([]PoolEntry{
			{Name: "p1", Provider: nil, Priority: 1},
		}, slog.Default(), nil)
		if err == nil {
			t.Error("want error for nil provider")
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		p := newMock("p", "ok", nil)
		_, err := NewPool([]PoolEntry{
			{Name: "p1", Provider: p, Priority: 1},
			{Name: "p1", Provider: p, Priority: 2},
		}, slog.Default(), nil)
		if err == nil {
			t.Error("want error for duplicate name")
		}
	})

	t.Run("empty name", func(t *testing.T) {
		p := newMock("p", "ok", nil)
		_, err := NewPool([]PoolEntry{
			{Name: "", Provider: p, Priority: 1},
		}, slog.Default(), nil)
		if err == nil {
			t.Error("want error for empty name")
		}
	})
}

func TestPool_DefaultCircuitBreakerWhenNil(t *testing.T) {
	p := newMock("p1", "ok", nil)
	entries := []PoolEntry{
		{Name: "p1", Provider: p, Priority: 1, CB: nil},
	}
	pool, err := NewPool(entries, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if pool.providers[0].CB == nil {
		t.Error("nil CB should be replaced with default")
	}
}

func TestPool_ConcurrentCalls(t *testing.T) {
	p1 := newMock("primary", "ok", nil)
	entries := []PoolEntry{
		{Name: "p1", Provider: p1, Priority: 1, CB: NewCircuitBreaker(100, 30*time.Second)},
	}
	pool, _ := NewPool(entries, slog.Default(), metrics.NewMetrics())

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = pool.DescribeImage(context.Background(), "data", "image/png", 100)
		}()
	}
	close(start)
	wg.Wait()

	if p1.callCount.Load() != 50 {
		t.Errorf("call count: want 50, got %d", p1.callCount.Load())
	}
}

func TestPool_ProviderNames(t *testing.T) {
	entries := []PoolEntry{
		{Name: "c", Provider: newMock("c", "", nil), Priority: 3, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "a", Provider: newMock("a", "", nil), Priority: 1, CB: NewCircuitBreaker(5, 30*time.Second)},
		{Name: "b", Provider: newMock("b", "", nil), Priority: 2, CB: NewCircuitBreaker(5, 30*time.Second)},
	}
	pool, _ := NewPool(entries, slog.Default(), nil)
	names := pool.ProviderNames()
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("names[%d]: want %q, got %q", i, w, names[i])
		}
	}
}
