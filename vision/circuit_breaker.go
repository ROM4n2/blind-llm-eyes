package vision

import (
	"fmt"
	"sync"
	"time"
)

// CBState 表示熔断器的三种状态。
type CBState int

const (
	CBClosed   CBState = iota // 正常运行，统计连续失败数
	CBOpen                    // 拒绝请求，等待 reset_timeout 后进入半开
	CBHalfOpen                // 放行一个试探请求，成功→Closed，失败→Open
)

// String 返回状态的可读名称，用于日志和指标。
func (s CBState) String() string {
	switch s {
	case CBClosed:
		return "closed"
	case CBOpen:
		return "open"
	case CBHalfOpen:
		return "half_open"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// NumericValue 返回状态的数值表示，用于 Prometheus gauge 指标。
func (s CBState) NumericValue() float64 {
	switch s {
	case CBClosed:
		return 0
	case CBOpen:
		return 1
	case CBHalfOpen:
		return 2
	default:
		return -1
	}
}

// CircuitBreaker 是一个线程安全的三态熔断器。
// 零值不可用，请使用 NewCircuitBreaker。
type CircuitBreaker struct {
	mu sync.Mutex

	state            CBState
	consecutiveFails int
	failureThreshold int
	resetTimeout     time.Duration
	openedAt         time.Time

	// halfOpenTrialInFlight 标记半开状态下是否已有一个试探请求在执行。
	// 保证半开态同时只有一个试探请求通过。
	halfOpenTrialInFlight bool
}

// NewCircuitBreaker 构造熔断器。failureThreshold<=0 时兜底为 5，resetTimeout<=0 时兜底为 30s。
func NewCircuitBreaker(failureThreshold int, resetTimeout time.Duration) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if resetTimeout <= 0 {
		resetTimeout = 30 * time.Second
	}
	return &CircuitBreaker{
		state:            CBClosed,
		failureThreshold: failureThreshold,
		resetTimeout:     resetTimeout,
	}
}

// Allow 检查是否允许请求通过。
// 返回 true 表示放行（调用方随后必须调用 RecordSuccess 或 RecordFailure）。
// 返回 false 表示拒绝（熔断器开启且未到重试时间，或半开态已有试探在执行）。
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case CBClosed:
		return true

	case CBOpen:
		// 检查 reset_timeout 是否已过，若已过则转入半开态并放行一个试探
		if time.Since(cb.openedAt) >= cb.resetTimeout {
			cb.state = CBHalfOpen
			cb.halfOpenTrialInFlight = true
			return true
		}
		return false

	case CBHalfOpen:
		// 半开态：仅允许一个试探请求
		if cb.halfOpenTrialInFlight {
			return false
		}
		cb.halfOpenTrialInFlight = true
		return true
	}

	return false
}

// RecordSuccess 记录一次成功调用，重置连续失败计数并关闭熔断器。
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFails = 0
	cb.state = CBClosed
	cb.halfOpenTrialInFlight = false
}

// RecordFailure 记录一次失败调用。
// Closed 态下连续失败数达到阈值时开启熔断器；Half-open 态下直接重新开启。
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveFails++

	switch cb.state {
	case CBClosed:
		if cb.consecutiveFails >= cb.failureThreshold {
			cb.state = CBOpen
			cb.openedAt = time.Now()
		}
	case CBHalfOpen:
		// 试探失败，重新开启熔断器
		cb.state = CBOpen
		cb.openedAt = time.Now()
		cb.halfOpenTrialInFlight = false
	}
}

// State 返回当前熔断器状态（线程安全，仅读取，不触发状态转移）。
func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// CBStats 是熔断器的内部状态快照，用于日志记录。
type CBStats struct {
	State            CBState
	ConsecutiveFails int
	FailureThreshold int
	ResetTimeout     time.Duration
	OpenedAgo        time.Duration // 熔断器开启后经过的时间（未开启时为 0）
	HalfOpenInFlight bool
}

// Stats 返回当前熔断器的完整状态快照（线程安全）。
// 用于日志记录，帮助排查熔断器行为。
func (cb *CircuitBreaker) Stats() CBStats {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	var openedAgo time.Duration
	if cb.state == CBOpen && !cb.openedAt.IsZero() {
		openedAgo = time.Since(cb.openedAt)
	}
	return CBStats{
		State:            cb.state,
		ConsecutiveFails: cb.consecutiveFails,
		FailureThreshold: cb.failureThreshold,
		ResetTimeout:     cb.resetTimeout,
		OpenedAgo:        openedAgo,
		HalfOpenInFlight: cb.halfOpenTrialInFlight,
	}
}
