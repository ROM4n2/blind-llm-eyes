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
	Priority int            // 数值越小优先级越高（越先尝试）
	CB       *CircuitBreaker // 可为 nil（单 provider 场景下不熔断）
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

// DescribeImage 实现 VisionProvider 接口。
// 按优先级遍历 provider，跳过熔断器开启的，在失败时故障转移到下一个。
// 所有 provider 都失败或熔断时返回错误。
func (p *Pool) DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error) {
	requestID := logging.GetRequestID(ctx)
	log := p.log.With("node_name", "vision_pool", "request_id", requestID)

	var lastErr error
	var triedProvider string

	for i := range p.providers {
		entry := &p.providers[i]
		triedProvider = entry.Name

		// 1. 检查熔断器
		allowed := entry.CB.Allow()
		if !allowed {
			cbState := entry.CB.State()
			log.Info("provider skipped (circuit breaker open)",
				"stage", "provider_skipped",
				"status", "info",
				"provider", entry.Name,
				"priority", entry.Priority,
				"cb_state", cbState.String(),
			)
			if p.m != nil {
				p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "skipped").Inc()
				p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(cbState.NumericValue())
			}
			lastErr = fmt.Errorf("provider %q circuit open", entry.Name)
			continue
		}

		// 2. 调用 provider
		pStart := time.Now()
		desc, err := entry.Provider.DescribeImage(ctx, base64Data, mediaType, imageSize)
		pElapsed := time.Since(pStart)

		// 3. 记录结果
		if err == nil {
			entry.CB.RecordSuccess()
			cbState := entry.CB.State()
			log.Info("provider succeeded",
				"stage", "provider_success",
				"status", "ok",
				"provider", entry.Name,
				"priority", entry.Priority,
				"duration_ms", pElapsed.Milliseconds(),
				"cb_state", cbState.String(),
				"desc_len", len(desc),
			)
			if p.m != nil {
				p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "success").Inc()
				p.m.ProviderDuration.WithLabelValues(entry.Name).Observe(pElapsed.Seconds())
				p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(cbState.NumericValue())
			}
			return desc, nil
		}

		// 失败：记录并尝试下一个 provider
		entry.CB.RecordFailure()
		cbState := entry.CB.State()
		isLast := i == len(p.providers)-1
		log.Warn("provider failed, failing over",
			"stage", "provider_failover",
			"status", "warning",
			"provider", entry.Name,
			"priority", entry.Priority,
			"duration_ms", pElapsed.Milliseconds(),
			"err", err,
			"cb_state", cbState.String(),
			"has_next_provider", !isLast,
		)
		if p.m != nil {
			p.m.ProviderCallsTotal.WithLabelValues(entry.Name, "error").Inc()
			p.m.ProviderDuration.WithLabelValues(entry.Name).Observe(pElapsed.Seconds())
			p.m.CircuitBreakerState.WithLabelValues(entry.Name).Set(cbState.NumericValue())
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
		"last_err", lastErr,
	)
	return "", fmt.Errorf("all %d providers failed or circuit-open (last: %w)", len(p.providers), lastErr)
}

// ProviderNames 返回池中所有 provider 的名称（按优先级顺序），用于启动日志。
func (p *Pool) ProviderNames() []string {
	names := make([]string, len(p.providers))
	for i := range p.providers {
		names[i] = p.providers[i].Name
	}
	return names
}
