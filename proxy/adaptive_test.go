package proxy

import (
	"testing"
	"time"

	"github.com/ROM4n2/blind-llm-eyes/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// tinyCooldown 配合 tinySleep，保证每「轮」只评估恰好 1 次，
// 同时不会让测试变慢（总睡眠 ≈ 2ms × N 轮）。
const tinyCooldown = int(1)

func tinySleep() { time.Sleep(2 * time.Millisecond) }

// TestAdaptive_Increase 验证快样本驱动 AI 加性增：
// initial=4, window=20, fast=8000. 20 个 5s 样本 → P90=5<8 → +1=5
// 再 20 个 → +1=6
func TestAdaptive_Increase(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        16,
		InitialLimit:    4,
		FastThresholdMs: 8000,
		SampleWindow:    20,
		CooldownMs:      tinyCooldown,
		IncreaseStep:    1,
	}, nil, nil)

	// 批 1：19 个不够；+ 第 20 个触发一次评估 → limit 4→5
	for i := 0; i < 19; i++ {
		ac.RecordSample(5000, false)
	}
	if got := ac.CurrentLimit(); got != 4 {
		t.Fatalf("after 19 samples limit=%d, want 4 (window not full yet)", got)
	}
	tinySleep()
	ac.RecordSample(5000, false)
	if got := ac.CurrentLimit(); got != 5 {
		t.Fatalf("after first window of 20 fast samples limit=%d, want 5", got)
	}

	// 批 2：再 20 个。前 19 个在 cooldown 内（不评估），最后 1 个 sleep 后恰好触发 1 次 → 5→6
	for i := 0; i < 20; i++ {
		if i == 19 {
			tinySleep()
		}
		ac.RecordSample(5000, false)
	}
	if got := ac.CurrentLimit(); got != 6 {
		t.Fatalf("after second window limit=%d, want 6", got)
	}
}

// TestAdaptive_Decrease 验证慢样本驱动 MD 乘性减，并被 min_limit=2 夹住：
// initial=16, ratio=0.75, min=2 → 序列：16→12→9→6→4→3→2→2→2
func TestAdaptive_Decrease(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        2,
		MaxLimit:        20,
		InitialLimit:    16,
		SlowThresholdMs: 15000,
		SampleWindow:    20,
		CooldownMs:      tinyCooldown,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.5, // 足够大，不会误触发错误分支
	}, nil, nil)

	// 一次性灌 19 个打底（不再触发评估），随后每轮用 1 个收尾样本触发恰好 1 次评估
	for i := 0; i < 19; i++ {
		ac.RecordSample(20000, false)
	}

	expected := []int{12, 9, 6, 4, 3, 2, 2, 2}
	for i, want := range expected {
		tinySleep()
		ac.RecordSample(20000, false) // 第 20 个，触发本轮 1 次评估
		got := ac.CurrentLimit()
		if got != want {
			t.Fatalf("round %d: limit=%d, want %d", i, got, want)
		}
		// 丢掉 1 个最老样本，让下一轮也正好需要 1 个新样本填满 20
		// （无法直接访问内部切片，改为追加 19 个新样本覆盖，下一轮最后 1 个触发）
		for j := 0; j < 19; j++ {
			ac.RecordSample(20000, false)
		}
	}
}

// TestAdaptive_ErrorTriggeredDecrease 验证错误率阈值触发下降，
// 即使 P90 在 [fast, slow] 滞回区里，只要错误率超阈值就下降。
func TestAdaptive_ErrorTriggeredDecrease(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        20,
		InitialLimit:    10,
		FastThresholdMs: 3000,
		SlowThresholdMs: 10000,
		SampleWindow:    20,
		CooldownMs:      tinyCooldown,
		DecreaseRatio:   0.8, // 10 × 0.8 = 8
		ErrorThreshold:  0.05, // 5%
	}, nil, nil)

	// 前 19 个：2 个错误 + 17 个正常
	errCount := 0
	for i := 0; i < 19; i++ {
		isErr := errCount < 2
		if isErr {
			errCount++
		}
		ac.RecordSample(6000, isErr)
	}
	// 第 20 个：正常样本，整批 errRate=2/20=10% > 5%，触发 MD
	tinySleep()
	ac.RecordSample(6000, false)
	if got := ac.CurrentLimit(); got != 8 {
		t.Fatalf("after error-rate-triggered decrease limit=%d, want 8", got)
	}
}

// TestAdaptive_DisabledIsStatic 验证 enabled=false 时 RecordSample 完全是 no-op。
func TestAdaptive_DisabledIsStatic(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:      false,
		InitialLimit: 7,
		MinLimit:     1,
		MaxLimit:     16,
	}, nil, nil)

	for i := 0; i < 100; i++ {
		ms := int64(5000 + (i%3)*10000)
		isErr := i%7 == 0
		ac.RecordSample(ms, isErr)
	}
	if got := ac.CurrentLimit(); got != 7 {
		t.Fatalf("disabled controller limit=%d, want 7 (unchanged)", got)
	}
}

// TestAdaptive_HysteresisBand 验证 P90 位于 [fast, slow] 之间、无错误 → 不调整。
func TestAdaptive_HysteresisBand(t *testing.T) {
	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        16,
		InitialLimit:    5,
		FastThresholdMs: 8000,
		SlowThresholdMs: 15000,
		SampleWindow:    20,
		CooldownMs:      tinyCooldown,
		IncreaseStep:    1,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.10,
	}, nil, nil)

	// 打底 19 个
	for i := 0; i < 19; i++ {
		ac.RecordSample(11000, false)
	}

	for round := 0; round < 3; round++ {
		tinySleep()
		ac.RecordSample(11000, false) // 每轮恰好 1 次评估
		if got := ac.CurrentLimit(); got != 5 {
			t.Fatalf("round %d hysteresis: limit=%d, want 5 (no change in middle band)", round, got)
		}
		// 19 个新鲜样本覆盖，为下一轮最后 1 个样本触发准备
		for j := 0; j < 19; j++ {
			ac.RecordSample(11000, false)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
//  辅助断言
// ──────────────────────────────────────────────────────────────────────────

func assertLimit(t *testing.T, ac *AdaptiveConcurrency, want int) {
	t.Helper()
	if got := ac.CurrentLimit(); got != want {
		t.Fatalf("CurrentLimit()=%d, want %d", got, want)
	}
}

func assertMetric(t *testing.T, metric prometheus.Collector, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(metric); got != want {
		t.Fatalf("metric value=%.4f, want %.4f", got, want)
	}
}

// ──────────────────────────────────────────────────────────────────────────
//  脚本场景复现：完整生命周期 + Prometheus 指标验证
// ──────────────────────────────────────────────────────────────────────────

// TestAdaptive_ScriptScenario_FullLifecycle 复现 test-adaptive.ps1 的端到端验证场景：
//
//   - 配置与脚本 [RECOMMENDED CONFIG] 一致：min=1, max=4, initial=4,
//     slow=3s, fast=1s, window=4, ratio=0.75
//   - 每轮注入 4 个 8s 样本（模拟 MiMo vision 调用），P90=8s >> slow=3s → MD 下降
//   - 验证 limit 序列：4 → 3 → 2 → 1 → 1（floor clamp）
//   - 验证 3 个 Prometheus 指标：current / adjustments(down·none) / P90
func TestAdaptive_ScriptScenario_FullLifecycle(t *testing.T) {
	m := metrics.NewMetrics()

	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        4,
		InitialLimit:    4,
		FastThresholdMs: 1000,  // MiMo 8s >> 1s → 永远不会触发 increase
		SlowThresholdMs: 3000,  // MiMo 8s > 3s → 每批都判定 tooSlow
		SampleWindow:    4,     // 1 轮 = 4 张图 = 正好填满窗口
		CooldownMs:      tinyCooldown,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.1,
	}, m, nil)

	const mimoMs = int64(8000) // 模拟 MiMo 调用耗时 8s

	// ── Phase 0: 初始状态（进程刚启动）
	assertLimit(t, ac, 4)
	assertMetric(t, m.AdaptiveConcurrencyCurrent, 4)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 0)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("none"), 0)

	// ── Phase 1: Round 1 — 3 个样本不触发评估，第 4 个填满窗口 → MD: 4×0.75=3
	for i := 0; i < 3; i++ {
		ac.RecordSample(mimoMs, false)
	}
	assertLimit(t, ac, 4) // 窗口未满，尚未评估
	tinySleep()
	ac.RecordSample(mimoMs, false) // 第 4 个 → 触发评估
	assertLimit(t, ac, 3)
	assertMetric(t, m.AdaptiveConcurrencyCurrent, 3)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 1)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("none"), 0)
	assertMetric(t, m.AdaptiveVisionP90Seconds, 8.0) // P90 = 8000ms = 8.0s

	// ── Phase 2: Round 2 — 滚动窗口，每个新样本都触发评估（cooldown 已过）
	// s5: 3×0.75=2.25 → floor=2
	tinySleep()
	ac.RecordSample(mimoMs, false)
	assertLimit(t, ac, 2)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 2)

	// s6: 2×0.75=1.5 → floor=1（触底 min_limit）
	tinySleep()
	ac.RecordSample(mimoMs, false)
	assertLimit(t, ac, 1)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 3)

	// ── Phase 3: Round 3 — 已在 floor=1，不能继续降 → direction="none"
	tinySleep()
	ac.RecordSample(mimoMs, false)
	assertLimit(t, ac, 1) // 不变
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("none"), 1)
	assertMetric(t, m.AdaptiveConcurrencyCurrent, 1)

	// ── Phase 4: Round 4 — 仍然在 floor，继续累积 "none"
	tinySleep()
	ac.RecordSample(mimoMs, false)
	assertLimit(t, ac, 1)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("none"), 2)

	// ── 最终汇总：与脚本输出一致
	//   脚本 Round 1: limit=3, adj(down)=1
	//   脚本 Round 2: limit=1, adj(down)=3
	//   脚本 Round 3: limit=1, adj(none)=4+（脚本每轮 4 个 eval，此处每轮 1 个）
	//   脚本 Round 4: limit=1, adj(none)=8+
	t.Logf("final: limit=1, down=3, none=2, P90=8.0s")
	assertMetric(t, m.AdaptiveConcurrencyCurrent, 1)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 3)
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("none"), 2)
	assertMetric(t, m.AdaptiveVisionP90Seconds, 8.0)
}

// TestAdaptive_ScriptScenario_MixedLatency 验证混合延迟场景下 P90 的正确性：
//   - 3 个快样本(500ms) + 1 个慢样本(8000ms)
//   - P90 = sorted[ceil(0.9×4)-1] = sorted[3] = 8000ms > slow=3000ms → MD 下降
//   - 验证 P90 指标反映的是第 90 百分位而非平均值
func TestAdaptive_ScriptScenario_MixedLatency(t *testing.T) {
	m := metrics.NewMetrics()

	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        4,
		InitialLimit:    4,
		FastThresholdMs: 1000,
		SlowThresholdMs: 3000,
		SampleWindow:    4,
		CooldownMs:      tinyCooldown,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.1,
	}, m, nil)

	// 3 快 + 1 慢：P90 应取第 4 高值 = 8000ms
	// 平均值 = (500×3 + 8000) / 4 = 2375ms < slow=3000ms
	// 如果用平均值而非 P90，就不会触发下降 — 这正是 P90 的意义
	ac.RecordSample(500, false)
	ac.RecordSample(500, false)
	ac.RecordSample(500, false)
	tinySleep()
	ac.RecordSample(8000, false) // 触发评估

	assertLimit(t, ac, 3)                     // P90=8s > slow=3s → MD 下降
	assertMetric(t, m.AdaptiveVisionP90Seconds, 8.0) // P90 = 8000ms = 8.0s
}

// TestAdaptive_ScriptScenario_ErrorRateTriggersDecrease 验证错误率触发下降
// 在脚本场景配置下（error_threshold=0.1）：
//   - 3 个正常(8s) + 1 个错误 → errRate=25% > 10% → MD 下降
//   - 即使 P90=8s > slow=3s 也会触发，但此处专门验证 error 分支
func TestAdaptive_ScriptScenario_ErrorRateTriggersDecrease(t *testing.T) {
	m := metrics.NewMetrics()

	ac := NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
		Enabled:         true,
		MinLimit:        1,
		MaxLimit:        4,
		InitialLimit:    4,
		FastThresholdMs: 1000,
		SlowThresholdMs: 30000, // 故意设很高，排除 tooSlow 分支，只测 error 分支
		SampleWindow:    4,
		CooldownMs:      tinyCooldown,
		DecreaseRatio:   0.75,
		ErrorThreshold:  0.1,
	}, m, nil)

	// 3 正常 + 1 错误 = 25% > 10%
	ac.RecordSample(8000, false)
	ac.RecordSample(8000, false)
	ac.RecordSample(8000, false)
	tinySleep()
	ac.RecordSample(8000, true) // 错误样本

	assertLimit(t, ac, 3) // error rate 25% > 10% → MD: 4×0.75=3
	assertMetric(t, m.AdaptiveConcurrencyAdjustments.WithLabelValues("down"), 1)
}
