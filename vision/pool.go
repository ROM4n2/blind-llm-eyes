package vision

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/logging"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
)

// PoolEntry 是 Provider 池中的一个条目，包含 provider 实例及其熔断器和优先级。
type PoolEntry struct {
	Name     string
	Provider VisionProvider
	Priority int             // 数值越小优先级越高（越先尝试）
	CB       *CircuitBreaker // 可为 nil（单 provider 场景下不熔断）
	// Timeout / LargeTimeout / LargeImageThreshold 用于在 Pool 层为每次 provider
	// 尝试创建独立的 WithTimeout 子 ctx，避免共享 parent deadline 饿死 fallback。
	// 都 <=0 时 Pool 不设独立超时，直接透传 parent ctx（由 provider 内部自行管理超时）。
	Timeout             time.Duration
	LargeTimeout        time.Duration
	LargeImageThreshold int64
}

// selectTimeout 按 imageSize 选择该 entry 的超时：大图用 LargeTimeout，否则 Timeout。
// 返回 0 表示不设独立超时。
func selectTimeout(entry *PoolEntry, imageSize int64) time.Duration {
	if entry.LargeImageThreshold > 0 && imageSize >= entry.LargeImageThreshold && entry.LargeTimeout > 0 {
		return entry.LargeTimeout
	}
	return entry.Timeout
}

// Pool 是多 Vision Provider 池，实现 VisionProvider 接口。
// 按优先级遍历 provider，跳过熔断器开启的 provider，在失败时自动故障转移到下一个。
// 零值不可用，请使用 NewPool。
type Pool struct {
	providers []PoolEntry // 按 priority 升序排列（稳定排序，同优先级保持配置顺序）
	log       *slog.Logger
	m         *metrics.Metrics
}

// NewPool 构造 Provider 池。entries 按 priority 升序稳定排序。
// log 可为 nil（兜底 slog.Default()）；m 可为 nil（跳过指标上报）。
func NewPool(entries []PoolEntry, log *slog.Logger, m *metrics.Metrics) (*Pool, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("pool requires at least 1 provider, got 0")
	}

	// 验证每个条目
	names := make(map[string]bool, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Name == "" {
			return nil, fmt.Errorf("provider[%d]: name is required", i)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("provider[%d]: duplicate name %q", i, e.Name)
		}
		names[e.Name] = true
		if e.Provider == nil {
			return nil, fmt.Errorf("provider[%d] %q: provider implementation is nil", i, e.Name)
		}
		// 熔断器为 nil 时创建一个默认的（永不真正熔断也行，但不如给一个默认的更一致）
		if e.CB == nil {
			e.CB = NewCircuitBreaker(5, 30*time.Second)
		}
	}

	// 稳定排序：按 priority 升序，同优先级保持原始配置顺序
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Priority < entries[j].Priority
	})

	if log == nil {
		log = slog.Default()
	}

	return &Pool{
		providers: entries,
		log:       log,
		m:         m,
	}, nil
}

// DescribeImage 实现 VisionProvider 接口（无上下文）。
// 按优先级遍历 provider，跳过熔断器开启的，在失败时故障转移到下一个。
// 等价于 DescribeImageWithContext(ctx, base64Data, mediaType, imageSize, "").
func (p *Pool) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	return p.describeImageInternal(ctx, base64Data, mediaType, imageSize, "")
}

// DescribeImageWithContext 带对话上下文调用 provider 池，实现 ContextualVisionProvider 接口。
// 若底层 provider 实现 ContextualVisionProvider 则带上下文调用，否则回退到 DescribeImage。
func (p *Pool) DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
	return p.describeImageInternal(ctx, base64Data, mediaType, imageSize, contextText)
}

// describeImageInternal 是池的内部实现，被 DescribeImage 和 DescribeImageWithContext 共用。
// 按优先级遍历 provider，熔断跳过，失败故障转移，日志/metrics/CB 统一处理。
func (p *Pool) describeImageInternal(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error) {
	requestID := logging.GetRequestID(ctx)
	log := p.log.With("node_name", "vision_pool", "request_id", requestID)

	poolStart := time.Now()

	// 日志：pool 开始 — 记录待处理的图片信息和可用 provider 列表
	log.Info("pool DescribeImage started",
		"stage", "pool_start",
		"status", "info",
		"image_size_bytes", imageSize,
		"media_type", mediaType,
		"provider_count", len(p.providers),
		"providers", p.ProviderNames(),
		"context_chars", len(contextText),
	)

	var lastErr error
	var triedProvider string
	failoverCount := 0

	for i := range p.providers {
		entry := &p.providers[i]
		triedProvider = entry.Name

		// 1. 检查熔断器 — 先获取快照用于转换检测
		statsBefore := entry.CB.Stats()
		allowed := entry.CB.Allow()
		if !allowed {
			statsAfter := entry.CB.Stats()
			log.Info("provider skipped (circuit breaker open)",
				"stage", "provider_skipped",
				"status", "info",
				"provider", entry.Name,
				"priority", entry.Priority,
				"cb_state", statsAfter.State.String(),
				"consecutive_fails", statsAfter.ConsecutiveFails,
				"failure_threshold", statsAfter.FailureThreshold,
				"opened_ago_ms", statsAfter.OpenedAgo.Milliseconds(),
				"reset_timeout_ms", statsAfter.ResetTimeout.Milliseconds(),
			)
			if p.m != nil {
				p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "skipped").Inc()
				p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(statsAfter.State.NumericValue())
			}
			lastErr = fmt.Errorf("provider %q circuit open", entry.Name)
			continue
		}

		// 检测 CB 状态转换（如 Open→HalfOpen）
		statsAfterAllow := entry.CB.Stats()
		if statsAfterAllow.State != statsBefore.State {
			log.Info("circuit breaker state transition",
				"stage", "cb_transition",
				"status", "info",
				"provider", entry.Name,
				"from_state", statsBefore.State.String(),
				"to_state", statsAfterAllow.State.String(),
				"consecutive_fails", statsAfterAllow.ConsecutiveFails,
				"failure_threshold", statsAfterAllow.FailureThreshold,
			)
		}

		// 2. 日志：provider 调用开始 — 记录调用前的上下文
		isContextual := false
		if _, ok := entry.Provider.(ContextualVisionProvider); ok {
			isContextual = true
		}
		// 为本次尝试选择独立 timeout：大图用 LargeTimeout，否则 Timeout。
		// Pool 层设独立 WithTimeout 子 ctx，避免共享 parent deadline 饿死 fallback。
		selectedTimeout := selectTimeout(entry, imageSize)
		log.Info("provider call started",
			"stage", "provider_call_start",
			"status", "info",
			"provider", entry.Name,
			"priority", entry.Priority,
			"cb_state", statsAfterAllow.State.String(),
			"image_size_bytes", imageSize,
			"provider_index", i,
			"total_providers", len(p.providers),
			"context_chars", len(contextText),
			"contextual", isContextual,
			"timeout_ms", selectedTimeout.Milliseconds(),
		)

		// 3. 调用 provider — 提取到 callProvider 方法，每次尝试用独立 WithTimeout
		//    子 ctx，且 defer cancel() 在方法返回时立即执行（避免循环内 defer 累积
		//    导致 ctx timer 存活到 describeImageInternal 结束的反模式）。
		desc, pErr, pElapsed := p.callProvider(ctx, entry, selectedTimeout, base64Data, mediaType, imageSize, contextText)
		err := pErr

		// 4. 记录结果
		if err == nil {
			statsBeforeRecord := entry.CB.Stats()
			entry.CB.RecordSuccess()
			statsAfterRecord := entry.CB.Stats()

			// 检测 CB 状态转换（如 HalfOpen→Closed）
			if statsAfterRecord.State != statsBeforeRecord.State {
				log.Info("circuit breaker recovered",
					"stage", "cb_recovered",
					"status", "ok",
					"provider", entry.Name,
					"from_state", statsBeforeRecord.State.String(),
					"to_state", statsAfterRecord.State.String(),
				)
			}

			log.Info("provider succeeded",
				"stage", "provider_success",
				"status", "ok",
				"provider", entry.Name,
				"priority", entry.Priority,
				"duration_ms", pElapsed.Milliseconds(),
				"cb_state", statsAfterRecord.State.String(),
				"desc_len", len(desc),
			)
			if p.m != nil {
				p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "success").Inc()
				p.m.ProviderDuration.WithLabelValues(entry.Name).Observe(pElapsed.Seconds())
				p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(statsAfterRecord.State.NumericValue())
			}

			// 日志：pool 完成 — 记录总耗时和成功 provider
			log.Info("pool DescribeImage completed",
				"stage", "pool_complete",
				"status", "ok",
				"total_duration_ms", time.Since(poolStart).Milliseconds(),
				"succeeded_provider", entry.Name,
				"providers_tried", i+1,
				"failover_count", failoverCount,
			)
			return desc, nil
		}

		// 失败：记录并尝试下一个 provider
		statsBeforeRecord := entry.CB.Stats()
		entry.CB.RecordFailure()
		statsAfterRecord := entry.CB.Stats()

		// 检测 CB 状态转换（如 Closed→Open 或 HalfOpen→Open）
		if statsAfterRecord.State != statsBeforeRecord.State {
			log.Warn("circuit breaker opened",
				"stage", "cb_opened",
				"status", "warning",
				"provider", entry.Name,
				"from_state", statsBeforeRecord.State.String(),
				"to_state", statsAfterRecord.State.String(),
				"consecutive_fails", statsAfterRecord.ConsecutiveFails,
				"failure_threshold", statsAfterRecord.FailureThreshold,
				"reset_timeout_ms", statsAfterRecord.ResetTimeout.Milliseconds(),
			)
		}

		isLast := i == len(p.providers)-1
		nextProvider := ""
		if !isLast {
			nextProvider = p.providers[i+1].Name
			failoverCount++
		}
		log.Warn("provider failed, failing over",
			"stage", "provider_failover",
			"status", "warning",
			"provider", entry.Name,
			"priority", entry.Priority,
			"duration_ms", pElapsed.Milliseconds(),
			"err", err.Error(),
			"cb_state", statsAfterRecord.State.String(),
			"consecutive_fails", statsAfterRecord.ConsecutiveFails,
			"failure_threshold", statsAfterRecord.FailureThreshold,
			"has_next_provider", !isLast,
			"next_provider", nextProvider,
		)
		if p.m != nil {
			p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "error").Inc()
			p.m.ProviderDuration.WithLabelValues(entry.Name).Observe(pElapsed.Seconds())
			p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(statsAfterRecord.State.NumericValue())
			if !isLast {
				p.m.FailoverEventsTotal.Inc()
			}
		}
		lastErr = fmt.Errorf("provider %q: %w", entry.Name, err)
	}

	// 所有 provider 都失败或被熔断
	log.Error("all providers failed or circuit-open",
		"stage", "pool_exhausted",
		"status", "error",
		"provider_count", len(p.providers),
		"last_provider", triedProvider,
		"last_err", lastErr.Error(),
		"total_duration_ms", time.Since(poolStart).Milliseconds(),
		"failover_count", failoverCount,
	)
	return "", fmt.Errorf("all %d providers failed or circuit-open (last: %w)", len(p.providers), lastErr)
}

// callProvider 调用单个 provider，用独立 WithTimeout 子 ctx 管理本次尝试的超时。
// 提取为独立方法确保 defer cancel() 在每次调用返回时立即执行，避免循环内 defer
// 累积（Go 反模式）：否则 N 个 provider 的 ctx timer 会存活到 describeImageInternal
// 结束才被清理。timeout <= 0 时直接透传 parent ctx（由 provider 内部自行管理超时）。
func (p *Pool) callProvider(ctx context.Context, entry *PoolEntry, timeout time.Duration, base64Data, mediaType string, imageSize int64, contextText string) (desc string, err error, elapsed time.Duration) {
	callCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	if cvp, ok := entry.Provider.(ContextualVisionProvider); ok {
		desc, err = cvp.DescribeImageWithContext(callCtx, base64Data, mediaType, imageSize, contextText)
	} else {
		desc, err = entry.Provider.DescribeImage(callCtx, base64Data, mediaType, imageSize)
	}
	return desc, err, time.Since(start)
}

// ProviderNames 返回池中所有 provider 的名称（按优先级顺序），用于启动日志。
func (p *Pool) ProviderNames() []string {
	names := make([]string, len(p.providers))
	for i := range p.providers {
		names[i] = p.providers[i].Name
	}
	return names
}
