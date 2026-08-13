package proxy

import (
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/metrics"
)

type sample struct {
	execMs int64
	isErr  bool
}

// AdaptiveConcurrencyCfg 与 config 同名字段对齐，proxy 内部使用的副本。
type AdaptiveConcurrencyCfg struct {
	Enabled         bool
	MinLimit        int
	MaxLimit        int
	InitialLimit    int
	FastThresholdMs int
	SlowThresholdMs int
	SampleWindow    int
	CooldownMs      int
	IncreaseStep    int
	DecreaseRatio   float64
	ErrorThreshold  float64
}

// AdaptiveConcurrency 根据 vision 调用延迟反馈动态调整 concurrency limit。
// 所有方法线程安全。zero value 不可用，请使用 NewAdaptiveConcurrency。
type AdaptiveConcurrency struct {
	cfg AdaptiveConcurrencyCfg
	m   *metrics.Metrics
	log *slog.Logger

	mu sync.Mutex

	samples    []sample
	limit      int
	lastAdjust time.Time
}

// NewAdaptiveConcurrency 构造控制器。如果 cfg.Enabled=false，控制器
// 依然可用但 CurrentLimit 始终返回 InitialLimit（= 静态值），RecordSample 是空操作。
// 对未设置的字段写入与 config.Load() 一致的默认值兜底，方便测试和 nil 注入场景。
// log 可为 nil，此时跳过日志输出。
func NewAdaptiveConcurrency(cfg AdaptiveConcurrencyCfg, m *metrics.Metrics, log *slog.Logger) *AdaptiveConcurrency {
	if cfg.InitialLimit <= 0 {
		cfg.InitialLimit = 6
	}
	if cfg.MinLimit <= 0 {
		cfg.MinLimit = 1
	}
	if cfg.MaxLimit <= 0 {
		cfg.MaxLimit = 12
	}
	if cfg.FastThresholdMs <= 0 {
		cfg.FastThresholdMs = 8000
	}
	if cfg.SlowThresholdMs <= 0 {
		cfg.SlowThresholdMs = 15000
	}
	if cfg.SampleWindow <= 0 {
		cfg.SampleWindow = 10
	}
	if cfg.IncreaseStep <= 0 {
		cfg.IncreaseStep = 1
	}
	if cfg.DecreaseRatio <= 0 || cfg.DecreaseRatio >= 1 {
		cfg.DecreaseRatio = 0.75
	}
	if cfg.ErrorThreshold <= 0 || cfg.ErrorThreshold >= 1 {
		cfg.ErrorThreshold = 0.10
	}
	// Min/Max 钳位
	if cfg.MinLimit > cfg.MaxLimit {
		cfg.MinLimit, cfg.MaxLimit = 1, 12
	}
	if cfg.InitialLimit < cfg.MinLimit {
		cfg.InitialLimit = cfg.MinLimit
	}
	if cfg.InitialLimit > cfg.MaxLimit {
		cfg.InitialLimit = cfg.MaxLimit
	}
	ac := &AdaptiveConcurrency{
		cfg:   cfg,
		m:     m,
		log:   log,
		limit: cfg.InitialLimit,
	}
	if m != nil {
		m.AdaptiveConcurrencyCurrent.Set(float64(ac.limit))
	}
	return ac
}

// CurrentLimit 返回当前有效 concurrency limit。
// 当 adaptive 关闭时恒等于 InitialLimit（静态配置值）。
func (a *AdaptiveConcurrency) CurrentLimit() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limit
}

// RecordSample 记录一次 vision 调用样本。
// 仅在 singleflight executor（shared=false）上调用，保证 fn_exec_ms 是真实 MiMo 耗时。
// 当 adaptive 关闭时是 no-op。
func (a *AdaptiveConcurrency) RecordSample(fnExecMs int64, isErr bool) {
	if !a.cfg.Enabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.samples = append(a.samples, sample{execMs: fnExecMs, isErr: isErr})
	if len(a.samples) > a.cfg.SampleWindow {
		a.samples = a.samples[1:]
	}

	if len(a.samples) < a.cfg.SampleWindow {
		return
	}
	if time.Since(a.lastAdjust) < time.Duration(a.cfg.CooldownMs)*time.Millisecond {
		return
	}

	// ── 评估入口：窗口已满 + cooldown 已过
	n := len(a.samples)
	sorted := make([]int64, 0, n)
	errs := 0
	for _, s := range a.samples {
		sorted = append(sorted, s.execMs)
		if s.isErr {
			errs++
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p90Idx := int(math.Ceil(0.9*float64(n))) - 1
	if p90Idx < 0 {
		p90Idx = 0
	}
	p90Ms := sorted[p90Idx]
	errRate := float64(errs) / float64(n)

	direction := "none"

	tooSlow := p90Ms > int64(a.cfg.SlowThresholdMs)
	tooManyErrors := errRate > a.cfg.ErrorThreshold
	tooFast := p90Ms < int64(a.cfg.FastThresholdMs) && errRate == 0

	switch {
	case tooSlow || tooManyErrors:
		newLimit := int(math.Floor(float64(a.limit) * a.cfg.DecreaseRatio))
		if newLimit < a.cfg.MinLimit {
			newLimit = a.cfg.MinLimit
		}
		oldLimit := a.limit
		if newLimit != a.limit {
			a.limit = newLimit
			direction = "down"
			reason := "too_slow"
			if tooManyErrors {
				reason = "error_rate_exceeded"
			}
			if a.log != nil {
				a.log.Info("adaptive: decreasing limit (MD)",
					"old_limit", oldLimit,
					"new_limit", newLimit,
					"reason", reason,
					"p90_ms", p90Ms,
					"slow_threshold_ms", a.cfg.SlowThresholdMs,
					"err_rate", errRate,
					"err_threshold", a.cfg.ErrorThreshold,
					"window_size", n,
					"ratio", a.cfg.DecreaseRatio,
				)
			}
		} else {
			// 已到 min_limit floor，无法继续下降
			if a.log != nil {
				a.log.Info("adaptive: at floor, cannot decrease further",
					"limit", a.limit,
					"min_limit", a.cfg.MinLimit,
					"reason", "floor_clamp",
					"p90_ms", p90Ms,
					"err_rate", errRate,
				)
			}
		}
	case tooFast:
		newLimit := a.limit + a.cfg.IncreaseStep
		if newLimit > a.cfg.MaxLimit {
			newLimit = a.cfg.MaxLimit
		}
		oldLimit := a.limit
		if newLimit != a.limit {
			a.limit = newLimit
			direction = "up"
			if a.log != nil {
				a.log.Info("adaptive: increasing limit (AI)",
					"old_limit", oldLimit,
					"new_limit", newLimit,
					"reason", "too_fast",
					"p90_ms", p90Ms,
					"fast_threshold_ms", a.cfg.FastThresholdMs,
					"err_rate", errRate,
					"window_size", n,
					"step", a.cfg.IncreaseStep,
				)
			}
		} else {
			// 已到 max_limit ceiling，无法继续上升
			if a.log != nil {
				a.log.Info("adaptive: at ceiling, cannot increase further",
					"limit", a.limit,
					"max_limit", a.cfg.MaxLimit,
					"reason", "ceiling_clamp",
					"p90_ms", p90Ms,
				)
			}
		}
	default:
		// 滞回区：fast ≤ P90 ≤ slow 且 错误率正常 → 不调整
		if a.log != nil {
			a.log.Info("adaptive: no change (hysteresis band)",
				"limit", a.limit,
				"p90_ms", p90Ms,
				"fast_threshold_ms", a.cfg.FastThresholdMs,
				"slow_threshold_ms", a.cfg.SlowThresholdMs,
				"err_rate", errRate,
				"window_size", n,
			)
		}
	}

	a.lastAdjust = time.Now()

	if a.m != nil {
		a.m.AdaptiveConcurrencyCurrent.Set(float64(a.limit))
		a.m.AdaptiveConcurrencyAdjustments.WithLabelValues(direction).Inc()
		a.m.AdaptiveVisionP90Seconds.Set(float64(p90Ms) / 1000.0)
	}
}
