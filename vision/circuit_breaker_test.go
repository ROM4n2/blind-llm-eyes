package vision

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreaker_StartsClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)
	if cb.State() != CBClosed {
		t.Fatalf("want closed, got %s", cb.State())
	}
}

func TestCircuitBreaker_ClosedAllowsAll(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)
	for i := 0; i < 10; i++ {
		if !cb.Allow() {
			t.Fatalf("attempt %d: closed should allow", i)
		}
	}
}

func TestCircuitBreaker_OpenDeniesUntilResetTimeout(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)

	// 触发熔断：3 次连续失败
	for i := 0; i < 3; i++ {
		if !cb.Allow() {
			t.Fatalf("attempt %d: should allow in closed", i)
		}
		cb.RecordFailure()
	}
	if cb.State() != CBOpen {
		t.Fatalf("want open after %d failures, got %s", 3, cb.State())
	}

	// 熔断开启：应拒绝
	if cb.Allow() {
		t.Fatal("open should deny before reset_timeout")
	}

	// 等待 reset_timeout 过后
	time.Sleep(60 * time.Millisecond)

	// 应转为半开并放行一个试探
	if !cb.Allow() {
		t.Fatal("should allow trial after reset_timeout (half-open)")
	}
	if cb.State() != CBHalfOpen {
		t.Fatalf("want half-open after reset_timeout, got %s", cb.State())
	}

	// 半开态：试探已在执行，第二个请求应被拒绝
	if cb.Allow() {
		t.Fatal("half-open should deny second concurrent trial")
	}
}

func TestCircuitBreaker_HalfOpenSuccessClosesCircuit(t *testing.T) {
	cb := NewCircuitBreaker(2, 30*time.Millisecond)

	// 开启熔断
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != CBOpen {
		t.Fatalf("want open, got %s", cb.State())
	}

	time.Sleep(35 * time.Millisecond)

	// 半开放行试探
	if !cb.Allow() {
		t.Fatal("should allow half-open trial")
	}

	// 试探成功 → 关闭
	cb.RecordSuccess()
	if cb.State() != CBClosed {
		t.Fatalf("want closed after half-open success, got %s", cb.State())
	}
	if cb.consecutiveFails != 0 {
		t.Fatalf("consecutiveFails should be 0, got %d", cb.consecutiveFails)
	}

	// 关闭后应正常放行
	if !cb.Allow() {
		t.Fatal("closed should allow after recovery")
	}
}

func TestCircuitBreaker_HalfOpenFailureReopensCircuit(t *testing.T) {
	cb := NewCircuitBreaker(2, 30*time.Millisecond)

	// 开启熔断
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()

	time.Sleep(35 * time.Millisecond)

	// 半开放行试探
	if !cb.Allow() {
		t.Fatal("should allow half-open trial")
	}

	// 试探失败 → 重新开启
	cb.RecordFailure()
	if cb.State() != CBOpen {
		t.Fatalf("want open after half-open failure, got %s", cb.State())
	}

	// 应拒绝
	if cb.Allow() {
		t.Fatal("open should deny after half-open failure")
	}
}

func TestCircuitBreaker_SuccessResetsConsecutiveFails(t *testing.T) {
	cb := NewCircuitBreaker(5, 50*time.Millisecond)

	// 2 次失败（未达阈值 5）
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()

	// 1 次成功 → 重置计数
	cb.Allow()
	cb.RecordSuccess()

	// 再 2 次失败不应触发熔断（计数从 0 重新开始）
	cb.Allow()
	cb.RecordFailure()
	cb.Allow()
	cb.RecordFailure()

	if cb.State() != CBClosed {
		t.Fatalf("want closed (count reset by success), got %s", cb.State())
	}
}

func TestCircuitBreaker_ConcurrentHalfOpenSingleTrial(t *testing.T) {
	cb := NewCircuitBreaker(1, 20*time.Millisecond)

	// 1 次失败即开启
	cb.Allow()
	cb.RecordFailure()
	if cb.State() != CBOpen {
		t.Fatalf("want open, got %s", cb.State())
	}

	time.Sleep(25 * time.Millisecond)

	// 并发 20 个 goroutine 调用 Allow，半开态应只放行 1 个。
	// 注意：放行的 goroutine 不立即 RecordSuccess（否则会关闭熔断器，
	// 后续 goroutine 在 Closed 态全部放行），而是持有试探槽直到所有 goroutine 检查完。
	var allowed, denied atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if cb.Allow() {
				allowed.Add(1)
				// 不调用 RecordSuccess/Failure — 持有试探槽
			} else {
				denied.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	// 所有 goroutine 完成后释放试探槽
	cb.RecordSuccess()

	if got := allowed.Load(); got != 1 {
		t.Errorf("want exactly 1 allowed trial in half-open, got %d", got)
	}
	if got := denied.Load(); got != 19 {
		t.Errorf("want 19 denied, got %d", got)
	}
}

func TestCircuitBreaker_DefaultsAppliedOnZero(t *testing.T) {
	cb := NewCircuitBreaker(0, 0)
	if cb.failureThreshold != 5 {
		t.Errorf("want default threshold 5, got %d", cb.failureThreshold)
	}
	if cb.resetTimeout != 30*time.Second {
		t.Errorf("want default reset_timeout 30s, got %v", cb.resetTimeout)
	}
}

func TestCBState_String(t *testing.T) {
	tests := []struct {
		state CBState
		want  string
	}{
		{CBClosed, "closed"},
		{CBOpen, "open"},
		{CBHalfOpen, "half_open"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("state %d: want %q, got %q", tt.state, tt.want, got)
		}
	}
}

func TestCBState_NumericValue(t *testing.T) {
	tests := []struct {
		state CBState
		want  float64
	}{
		{CBClosed, 0},
		{CBOpen, 1},
		{CBHalfOpen, 2},
	}
	for _, tt := range tests {
		if got := tt.state.NumericValue(); got != tt.want {
			t.Errorf("state %d: want %v, got %v", tt.state, tt.want, got)
		}
	}
}
