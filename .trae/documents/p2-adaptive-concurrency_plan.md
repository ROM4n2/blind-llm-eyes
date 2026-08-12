# P2 自适应限流实施计划

> 状态：草案待审批 | 目标：根据 MiMo 真实响应时间动态调整 `concurrency_limit`，响应慢时降并发防打爆，响应快时升并发提吞吐

***

## 1. 背景与问题

### 1.1 现状

当前 `concurrency_limit` 是静态值（`config.yaml` 配置后进程生命周期内固定，默认 4）。这在 MiMo 服务端波动时存在两个问题：

* **MiMo 变慢（故障 / 排队 / 大图）**：固定高并发会把请求堆在 MiMo 端，出现 30s+ 长尾，甚至触发 60s 超时

* **MiMo 很快（小图 / 缓存重建后轻量请求）**：固定低并发浪费吞吐潜力，5+ 图场景可以跑更高并发

### 1.2 为什么不能依赖 static + 人工调参

* MiMo TTFB 实测 8s 起，波动范围大（8s → 38s 都出现过，见 memory）

* 不同用户图大小差异极大（20KB 截图 vs 2MB 实拍）

* 单用户典型场景是 1-2 图，但极端场景会有 10+ 图粘贴

### 1.3 目标

* 静态 `concurrency_limit` 成为 **初始值 / 上限**，运行时由控制器动态调整

* 保留 `enabled: false` 开关，关闭时与当前行为 **100% 一致**（保证回滚路径）

* 变化平滑，不震荡（AIMD 风格，加性增、乘性减）

* 控制器状态跨请求共享（在 `requestHandler` singleton 上），而非每请求重置

***

## 2. 算法设计：AIMD + P90 反馈

### 2.1 核心思路（TCP 拥塞控制风格）

```
每次真实 vision 调用完成（singleflight executor，shared=false）
  → 把 fn_exec_ms 加入滚动窗口
  → 当满足「样本数 ≥ sample_window AND 距上次调整 ≥ cooldown_ms」：
      计算窗口内 P90 延迟 + 错误率
      ┌─ P90 > slow_threshold_ms OR 错误率 > error_threshold
      │    → limit = max(min_limit, floor(limit × decrease_ratio))  乘性减
      ├─ P90 < fast_threshold_ms AND 错误率 == 0
      │    → limit = min(max_limit, limit + increase_step)            加性增
      └─ 否则
           → 不变（滞回区，避免边界抖动）
```

### 2.2 为什么用 P90 而不是平均值

* 平均值会被 occasional 快调用掩盖长尾

* P90 能真实反映「大部分用户」感受的慢，保护尾部延迟

* 小样本（window=20）下 P90 实现简单：排序后取第 ceil(0.9×N) 个

### 2.3 为什么只采 singleflight executor 样本

* `visionMs`（SF.Do 总耗时）对等待者来说是 **等待时间**，不反映 MiMo 本身速度

* `fn_exec_ms`（SF fn 内部耗时）才是 **真实 MiMo 调用时间**，且每个 hash 组只产生 1 条，不会因为有 5 个等待者就把 1 次 MiMo 数据放大 5 倍

* 判断条件：`if !shared`（当前 goroutine 是 SF fn 的执行者）

### 2.4 默认参数（保守起步，后续通过真实数据调）

| 参数                  | 默认值                    | 说明                             |
| ------------------- | ---------------------- | ------------------------------ |
| `enabled`           | `false`                | **默认关闭**，用户主动打开才生效             |
| `min_limit`         | `1`                    | 降并发的地板                         |
| `max_limit`         | `16`                   | 升并发的天花板（约等于 4× 默认值）            |
| `initial_limit`     | `cfg.ConcurrencyLimit` | 初始值 = 静态配置，向后兼容                |
| `fast_threshold_ms` | `8000`                 | P90 < 8s → 认为「还有余量」            |
| `slow_threshold_ms` | `15000`                | P90 > 15s → 认为「过载了」            |
| `sample_window`     | `20`                   | 滚动窗口样本数（\~20 次 MiMo 调一次决策）     |
| `cooldown_ms`       | `3000`                 | 两次调整的最小间隔（防止频繁抖动）              |
| `increase_step`     | `1`                    | 每次 +1（保守，AIMD AI）              |
| `decrease_ratio`    | `0.75`                 | 每次 × 0.75（AIMD MD，比 ×0.5 温和一点） |
| `error_threshold`   | `0.10`                 | 错误率 > 10% → 触发降并发              |

### 2.5 错误分类

* **计入错误率**：`vision call` 返回的 `verr != nil`（真实 vision error，含超时、5xx）

* **不计入**：fail\_open 写入后业务继续，但样本会被计为错误（因为它确实说明 MiMo 不可靠）

### 2.6 线程安全

* `AdaptiveConcurrency` 内部用 `sync.Mutex` 保护窗口切片 + 调整状态

* 每次 vision 调用完成后 `RecordSample(execMs int64, isErr bool)` 一个方法

* 每次请求开始时 `CurrentLimit() int` 读当前值（mutex 读，因为要和写互斥；如果性能有顾虑可换 `atomic.Int64` 存 limit 单独读，但 mutex 方案更简单直接，window=20 量级无性能问题）

***

## 3. 文件改动清单

### 3.1 改动文件（6 个） + 新增文件（2 个）

| # | 文件                                  | 类型     | 改动摘要                                                                                                                                                 |
| - | ----------------------------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1 | `config/loader.go`                  | 改      | 新增 `AdaptiveConcurrencyCfg` struct，加到 `Config` 顶层，写默认值                                                                                               |
| 2 | `config.example.yaml`               | 改      | 追加 `adaptive_concurrency:` 节，含全部默认参数与注释                                                                                                              |
| 3 | `metrics/metrics.go`                | 改      | 新增 3 个 Prometheus 指标（current\_limit gauge / adjustments counter / vision\_p90 gauge）                                                                 |
| 4 | `proxy/adaptive.go`                 | **新增** | `AdaptiveConcurrency` 控制器实现（RecordSample / CurrentLimit / 决策逻辑）                                                                                      |
| 5 | `proxy/handler.go`                  | 改      | 1) `HandlerDeps` 加 `AdaptiveEnabled` 配置；2) `requestHandler` 加 `ac *AdaptiveConcurrency`；3) g.SetLimit 改为读当前值；4) SF 完成后 `if !shared` 调 `RecordSample` |
| 6 | `main.go`                           | 改      | 1) 构造 `AdaptiveConcurrency` 并注入；2) 启动日志打 adaptive 配置；3) 可选：给 Metrics 指针                                                                              |
| 7 | `proxy/adaptive_test.go`            | **新增** | 控制器单元测试（3 场景：升 / 降 / 滞回）                                                                                                                             |
| 8 | `proxy/handler_concurrency_test.go` | 改      | 追加跨请求自适应集成测试（构造 3 批请求，mock 延迟递增，验证 limit 实际下降驱动第三批串行化）                                                                                               |

***

## 4. 详细设计与变更步骤

### 4.1 `config/loader.go`：配置扩展

在 `Config` 同级新增：

```go
type AdaptiveConcurrencyCfg struct {
    Enabled          bool   `yaml:"enabled"`
    MinLimit         int    `yaml:"min_limit"`
    MaxLimit         int    `yaml:"max_limit"`
    FastThresholdMs  int    `yaml:"fast_threshold_ms"`
    SlowThresholdMs  int    `yaml:"slow_threshold_ms"`
    SampleWindow     int    `yaml:"sample_window"`
    CooldownMs       int    `yaml:"cooldown_ms"`
    IncreaseStep     int    `yaml:"increase_step"`
    DecreaseRatio    float64 `yaml:"decrease_ratio"` // 0.0~1.0，乘以 limit
    ErrorThreshold   float64 `yaml:"error_threshold"`  // 0.0~1.0
}
```

在 `Config` 中加字段：

```go
AdaptiveConcurrency AdaptiveConcurrencyCfg `yaml:"adaptive_concurrency"`
```

在 `Load()` 最后（return 之前）写默认值（**注意：只在 Enabled=true 缺失时也写默认值，保证用户只写** **`enabled: true`** **其他字段省略也能跑**）：

```go
if c.AdaptiveConcurrency.MinLimit <= 0 {
    c.AdaptiveConcurrency.MinLimit = 1
}
if c.AdaptiveConcurrency.MaxLimit <= 0 {
    c.AdaptiveConcurrency.MaxLimit = 16
}
if c.AdaptiveConcurrency.FastThresholdMs <= 0 {
    c.AdaptiveConcurrency.FastThresholdMs = 8000
}
if c.AdaptiveConcurrency.SlowThresholdMs <= 0 {
    c.AdaptiveConcurrency.SlowThresholdMs = 15000
}
if c.AdaptiveConcurrency.SampleWindow <= 0 {
    c.AdaptiveConcurrency.SampleWindow = 20
}
if c.AdaptiveConcurrency.CooldownMs <= 0 {
    c.AdaptiveConcurrency.CooldownMs = 3000
}
if c.AdaptiveConcurrency.IncreaseStep <= 0 {
    c.AdaptiveConcurrency.IncreaseStep = 1
}
if c.AdaptiveConcurrency.DecreaseRatio <= 0 || c.AdaptiveConcurrency.DecreaseRatio >= 1 {
    c.AdaptiveConcurrency.DecreaseRatio = 0.75
}
if c.AdaptiveConcurrency.ErrorThreshold <= 0 || c.AdaptiveConcurrency.ErrorThreshold >= 1 {
    c.AdaptiveConcurrency.ErrorThreshold = 0.10
}
// 参数合理性校验（防止用户配错）
if c.AdaptiveConcurrency.MinLimit > c.AdaptiveConcurrency.MaxLimit {
    return nil, fmt.Errorf("adaptive_concurrency: min_limit(%d) > max_limit(%d)",
        c.AdaptiveConcurrency.MinLimit, c.AdaptiveConcurrency.MaxLimit)
}
if c.AdaptiveConcurrency.FastThresholdMs >= c.AdaptiveConcurrency.SlowThresholdMs {
    return nil, fmt.Errorf("adaptive_concurrency: fast_threshold_ms(%d) must be < slow_threshold_ms(%d)",
        c.AdaptiveConcurrency.FastThresholdMs, c.AdaptiveConcurrency.SlowThresholdMs)
}
```

### 4.2 `config.example.yaml`：配置模板追加

```yaml
# 自适应并发控制（默认关闭）。开启后会根据 MiMo 实际响应时间在 [min_limit, max_limit]
# 区间内动态调整 concurrency_limit。关闭时完全使用上面的静态 concurrency_limit 值。
# 算法：AIMD 风格 — P90 延迟低于 fast_threshold 时 +increase_step；高于 slow_threshold
# 或错误率 > error_threshold 时 ×decrease_ratio；中间滞回区不调整。
adaptive_concurrency:
  enabled: false                      # 生产验证后再改为 true
  min_limit: 1                        # 降并发地板（至少 1 路）
  max_limit: 16                       # 升并发天花板（4× 默认值）
  fast_threshold_ms: 8000             # P90 < 8s → 认为有升并发空间
  slow_threshold_ms: 15000            # P90 > 15s → 认为过载，降并发
  sample_window: 20                   # 滚动窗口样本数（~20 次 vision 调用评一次）
  cooldown_ms: 3000                   # 两次调整之间的最小间隔（防抖动）
  increase_step: 1                    # 每次升并发 +N
  decrease_ratio: 0.75                # 每次降并发 ×ratio（0.75 = 降 25%）
  error_threshold: 0.10               # 错误率 > 10% → 触发降并发
```

### 4.3 `metrics/metrics.go`：新增 3 个指标

```go
// 自适应限流指标（仅当 adaptive 启用时会有非默认值）
AdaptiveConcurrencyCurrent prometheus.Gauge   // 当前有效 concurrency_limit
AdaptiveConcurrencyAdjustments *prometheus.CounterVec // {direction="up|down|none"} 累计调整次数
AdaptiveVisionP90Seconds     prometheus.Gauge   // 最近一次决策窗口的 P90（秒），便于 Prometheus 观察
```

`NewMetrics()` 中的具体定义：

```go
AdaptiveConcurrencyCurrent: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
    Name: "blind_llm_eyes_adaptive_concurrency_current",
    Help: "Current effective concurrency limit. Equals static value when adaptive disabled.",
}),
AdaptiveConcurrencyAdjustments: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
    Name: "blind_llm_eyes_adaptive_concurrency_adjustments_total",
    Help: "Total adaptive concurrency adjustments, labeled by direction.",
}, []string{"direction"}), // "up" | "down" | "none"
AdaptiveVisionP90Seconds: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
    Name: "blind_llm_eyes_adaptive_vision_p90_seconds",
    Help: "P90 vision latency (seconds) from the most recent evaluation window.",
}),
```

### 4.4 `proxy/adaptive.go`（新文件）：控制器核心实现

```go
package proxy

import (
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

// AdaptiveConcurrency 根据 vision 调用延迟反馈动态调整 concurrency limit。
// 所有方法线程安全。zero value 不可用，请使用 NewAdaptiveConcurrency。
type AdaptiveConcurrency struct {
    cfg  AdaptiveConcurrencyCfg // 本地副本，来自 config
    m    *metrics.Metrics       // 可能为 nil

    mu sync.Mutex

    // 窗口
    samples []sample        // 固定容量 = cfg.SampleWindow 的环形缓冲？为了 P90 简单实现直接用切片 + len 截断
    // 当前 limit
    limit int
    // 上次调整时间（冷却用）
    lastAdjust time.Time
}

// AdaptiveConcurrencyCfg 与 config 同名字段对齐，proxy 内部使用的副本。
// 保持与 config 包中的 AdaptiveConcurrencyCfg 字段一致。
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

// NewAdaptiveConcurrency 构造控制器。如果 cfg.Enabled=false，控制器
// 依然可用但 CurrentLimit 始终返回 InitialLimit（= 静态值），RecordSample 是空操作。
func NewAdaptiveConcurrency(cfg AdaptiveConcurrencyCfg, m *metrics.Metrics) *AdaptiveConcurrency {
    if cfg.InitialLimit <= 0 {
        cfg.InitialLimit = 4
    }
    ac := &AdaptiveConcurrency{
        cfg:   cfg,
        m:     m,
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

    // 加入窗口（超出容量时丢最老的）
    a.samples = append(a.samples, sample{execMs: fnExecMs, isErr: isErr})
    if len(a.samples) > a.cfg.SampleWindow {
        a.samples = a.samples[1:]
    }

    // 窗口未满 / 冷却中 → 不做决策
    if len(a.samples) < a.cfg.SampleWindow {
        return
    }
    if time.Since(a.lastAdjust) < time.Duration(a.cfg.CooldownMs)*time.Millisecond {
        return
    }

    // 计算 P90 + 错误率
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
    p90Idx := int(math.Ceil(0.9 * float64(n))) - 1 // ceil(0.9*N) 的索引（从 0 起）
    if p90Idx < 0 {
        p90Idx = 0
    }
    p90Ms := sorted[p90Idx]
    errRate := float64(errs) / float64(n)

    // 决策
    direction := "none"
    oldLimit := a.limit

    tooSlow := p90Ms > int64(a.cfg.SlowThresholdMs)
    tooManyErrors := errRate > a.cfg.ErrorThreshold
    tooFast := p90Ms < int64(a.cfg.FastThresholdMs) && errRate == 0

    switch {
    case tooSlow || tooManyErrors:
        newLimit := int(math.Floor(float64(a.limit) * a.cfg.DecreaseRatio))
        if newLimit < a.cfg.MinLimit {
            newLimit = a.cfg.MinLimit
        }
        if newLimit != a.limit {
            a.limit = newLimit
            direction = "down"
        }
    case tooFast:
        newLimit := a.limit + a.cfg.IncreaseStep
        if newLimit > a.cfg.MaxLimit {
            newLimit = a.cfg.MaxLimit
        }
        if newLimit != a.limit {
            a.limit = newLimit
            direction = "up"
        }
    }

    a.lastAdjust = time.Now()

    // 更新指标
    if a.m != nil {
        a.m.AdaptiveConcurrencyCurrent.Set(float64(a.limit))
        a.m.AdaptiveConcurrencyAdjustments.WithLabelValues(direction).Inc()
        a.m.AdaptiveVisionP90Seconds.Set(float64(p90Ms) / 1000.0)
    }

    // 注意：这里不打日志，因为调用方（handler.go）已有完整日志链，且日志
    // 调整事件在 handler 侧打更方便带 request_id。如果需要独立 controller
    // 日志，后面再加 logger 依赖。
}
```

### 4.5 `proxy/handler.go`：接入控制器

**变更 A — HandlerDeps 加字段**

在 `HandlerDeps`（第 29-40 行）追加：

```go
AdaptiveConcurrency *AdaptiveConcurrency // 自适应限流控制器；nil 等价于 static 行为
```

**变更 B — requestHandler 持有指针**

`requestHandler` struct 里不用加，`deps` 已经持有。但要改 `NewHandler` 兜底（如果 deps.AdaptiveConcurrency == nil，构造一个 disabled 的，避免 handler 里到处 nil check）：

```go
func NewHandler(deps HandlerDeps) http.Handler {
    // ... 原有兜底 ...
    if deps.AdaptiveConcurrency == nil {
        deps.AdaptiveConcurrency = NewAdaptiveConcurrency(AdaptiveConcurrencyCfg{
            Enabled:      false,
            InitialLimit: deps.ConcurrencyLimit,
            MinLimit:     deps.ConcurrencyLimit,
            MaxLimit:     deps.ConcurrencyLimit,
        }, deps.Metrics)
    }
    // ...
}
```

**变更 C — g.SetLimit 改为读动态值**

第 159 行附近，原来的：

```go
g.SetLimit(h.deps.ConcurrencyLimit)
```

改为：

```go
effectiveLimit := h.deps.AdaptiveConcurrency.CurrentLimit()
g.SetLimit(effectiveLimit)
```

并在 `parallel_images_start` 日志里加字段（第 162-168 行）：

```go
log.Info("parallel image processing started",
    // ... 原有 ...
    "concurrency_limit", h.deps.ConcurrencyLimit, // 静态配置值（参考）
    "effective_limit", effectiveLimit,            // 实际生效值（自适应）
    // ...
)
```

**变更 D — vision 完成后上报样本**

在 handler.go 第 268 行附近（singleflight 日志之后），新增：

```go
// 只让 singleflight executor（非等待者）上报 fn_exec_ms 样本，
// 避免 1 次真实 vision 调用被 N 个等待者重复放大
if !shared {
    isVisonErr := verr != nil
    h.deps.AdaptiveConcurrency.RecordSample(fnExecMs, isVisonErr)
}
```

（注意：`verr` 在这个作用域里是 `h.sf.Do` 的 error 返回值，`fnExecMs` 是 SF fn 真实执行时间，两者都是当前作用域已有变量。）

### 4.6 `main.go`：启动时构造并注入

在 `deps := proxy.HandlerDeps{...}`（第 61-82 行）之前，先构造 adaptive controller：

```go
// 构造自适应并发控制器
acCfg := proxy.AdaptiveConcurrencyCfg{
    Enabled:         cfg.AdaptiveConcurrency.Enabled,
    MinLimit:        cfg.AdaptiveConcurrency.MinLimit,
    MaxLimit:        cfg.AdaptiveConcurrency.MaxLimit,
    InitialLimit:    cfg.ConcurrencyLimit,
    FastThresholdMs: cfg.AdaptiveConcurrency.FastThresholdMs,
    SlowThresholdMs: cfg.AdaptiveConcurrency.SlowThresholdMs,
    SampleWindow:    cfg.AdaptiveConcurrency.SampleWindow,
    CooldownMs:      cfg.AdaptiveConcurrency.CooldownMs,
    IncreaseStep:    cfg.AdaptiveConcurrency.IncreaseStep,
    DecreaseRatio:   cfg.AdaptiveConcurrency.DecreaseRatio,
    ErrorThreshold:  cfg.AdaptiveConcurrency.ErrorThreshold,
}
ac := proxy.NewAdaptiveConcurrency(acCfg, m)
```

然后在 deps 里加：

```go
deps := proxy.HandlerDeps{
    // ... 原有字段 ...
    AdaptiveConcurrency: ac,
}
```

启动日志追加字段（第 38-50 行）：

```go
logger.Info("blind-llm-eyes starting",
    // ... 原有 ...
    "concurrency_limit", cfg.ConcurrencyLimit,
    "adaptive_enabled", cfg.AdaptiveConcurrency.Enabled,
    "adaptive_range", fmt.Sprintf("[%d, %d]", cfg.AdaptiveConcurrency.MinLimit, cfg.AdaptiveConcurrency.MaxLimit),
    "adaptive_threshold_ms", fmt.Sprintf("fast=%d slow=%d",
        cfg.AdaptiveConcurrency.FastThresholdMs, cfg.AdaptiveConcurrency.SlowThresholdMs),
)
```

### 4.7 `proxy/adaptive_test.go`（新文件）：控制器单元测试

3 个核心测试 + 1 个 disabled 回归测试：

**Test A — AdaptiveIncrease：** 连续喂 20 个快样本（5s，低于 fast=8s），验证 limit 从 4 升到 5

* 构造 cfg：initial=4, window=20, cooldown=0, fast=8000, slow=15000

* 喂 19 个 5000ms：窗口未满，不应调整（limit 仍为 4）

* 喂第 20 个：触发评估，P90≈5s < fast=8s → limit += 1 → 5

* 再喂 20 个 5000ms：limit → 6

* 验证最终 CurrentLimit() == 6（两次增加）

**Test B — AdaptiveDecrease：** 连续喂 20 个慢样本（20s，高于 slow=15s），验证 limit 从 16 → 12

* initial=16, decrease\_ratio=0.75, window=20, cooldown=0

* 20×20000ms → P90≈20s > slow=15s → limit = floor(16×0.75) = 12

* 再喂 20 个：floor(12×0.75) = 9；再 20：6；再 20：4（但 min=2，到 2 后 stop）

* 最后验证 limit 被夹到 min

**Test C — AdaptiveErrorTriggeredDecrease：** window=20，塞 2 个错误（errRate=10% > threshold=8% ？实际用 threshold=0.05 触发），验证即使 P90 正常也触发 decrease

* threshold=0.05，20 样本里 2 个错误（errRate=0.10）

* P90=6s（正常），但错误率 > 0.05 → 依然乘性减

**Test D — DisabledIsStatic：** `enabled=false` 时，RecordSample 无论喂什么都不改变 limit

* enabled=false, initial=7

* 喂 100 个各种组合样本

* CurrentLimit() 仍 == 7

**Test E — HysteresisBand（滞回区）：** P90 在 \[fast, slow] 之间 → 不调整

* fast=8s, slow=15s，喂 P90≈11s 样本（middle band）

* limit 不变

### 4.8 `proxy/handler_concurrency_test.go`：跨请求集成测试

新增 `TestHandler_AdaptiveConcurrency_DecreasesAcrossRequests`：

```
目标：验证自适应限流在 handler 层真实生效（而非仅 controller 单测）

步骤：
1. 构造 adaptive cfg：enabled=true, initial=4, min=1, max=4,
                      sample_window=4, cooldown=0,
                      slow_threshold_ms=100（故意小，保证触发）
2. mock vision：每张图 delay=300ms（> slow=100ms）
3. 连续发 2 个请求，每个 4 张不同图（确保无 cache / 无 SF 去重，executor 数量充足）
   - 请求 1：4 张图 → 产生 4 个样本（够填满 window=4），最后 1 张 SF executor 返回前触发评估
     → limit 4 → floor(4×0.75)=3
   - 请求 2：4 张图，effective limit 应 =3
     → 观察 mock.offsets()：第 4 个调用的 start offset 应 >= 300ms（第一批 3 个并发，第 4 个等第一批完成）
4. 断言：请求 2 的 offsets[3] >= 280ms（即 limit=3 生效，不是 limit=4 的全并发）
```

用已有的 `slowVisionMock` + `buildNImageRequest` + `fakeUpstream`（同文件已有）即可，无需新基础设施。

***

## 5. 风险处理

| 风险                                     | 概率 | 影响 | 缓解                                                                    |
| -------------------------------------- | -- | -- | --------------------------------------------------------------------- |
| 初始参数不合理，开局就压垮 MiMo                     | 中  | 高  | **默认关闭**（`enabled: false`），用户主动开；`max_limit=16` 上限保护                  |
| 调整太快引起震荡（1 秒内升降多次）                     | 中  | 中  | `cooldown_ms=3000` + 滞回区（中间带不调整）+ 加性增每次仅 +1                           |
| singleflight 极端情况下 executor 挂掉，无人上报样本  | 低  | 低  | 没有样本就不做决策（graceful degradation），limit 停留在上次值，不会无限增大；冷启动用 initial      |
| 样本全是缓存命中（极端缓存命中），控制器没有新鲜数据             | 低  | 低  | cache hit 路径本就很快，不需要调；保持 initial limit 即可；只有 miss→vision→executor 才上报 |
| 多线程数据竞争                                | 低  | 高  | `RecordSample` 和 `CurrentLimit` 全程用 mutex，`go test -race` 必跑          |
| 历史测试因为注入了 `nil AdaptiveConcurrency` 而崩 | 中  | 中  | `NewHandler` 里 nil→构造 disabled fallback（initial=static 值），所有老测试无需改代码  |

***

## 6. 验证与验收

### 6.1 自动化测试

```powershell
# 全量 + race
go test -race -count=1 ./...

# 针对性跑自适应
go test -race -v -run "TestAdaptive|TestHandler_Adaptive" ./proxy/
```

### 6.2 手动验证（端到端）

1. 在 `config.yaml` 追加：

   ```yaml
   adaptive_concurrency:
     enabled: true
     min_limit: 1
     max_limit: 8
     sample_window: 5        # 小窗口，便于快速观察
     cooldown_ms: 1000
   ```
2. `go build -o blind-llm-eyes.exe . && .\blind-llm-eyes.exe`
3. 启动日志应包含 `adaptive_enabled=true` 和参数
4. 连续粘贴 5-10 张大图（从微信 / 截图拖入，保证触发 vision miss）
5. 观察日志里 `effective_limit` 字段变化：

   * 前几请求应该是 initial=4

   * 若 MiMo 快 → 几次后 effective\_limit=5

   * 若 MiMo 慢（>15s）→ 降到 3
6. `curl http://127.0.0.1:8790/metrics | findstr adaptive` 看 3 个新指标有值

### 6.3 关闭开关回归（最重要的安全网）

把 `enabled: false` 改回（或删节），重复一次同样的请求，确认：

* 所有日志 `effective_limit == concurrency_limit == 4`（初始值）

* limit 在整个会话内不变

* 行为与部署此 feature 前 **完全一致**

***

## 7. 实施步骤（按顺序执行）

1. **config/loader.go** 加 AdaptiveConcurrencyCfg struct + 默认值 + 校验
2. **config.example.yaml** 追加 adaptive\_concurrency 节
3. **metrics/metrics.go** 加 3 个新指标定义
4. **proxy/adaptive.go** 新建：AdaptiveConcurrency 控制器 + AdaptiveConcurrencyCfg 代理配置
5. **proxy/adaptive\_test.go** 新建：5 个单元测试（A\~E）→ 跑通 controller 单测
6. **proxy/handler.go**：HandlerDeps 加字段 / NewHandler nil 兜底 / g.SetLimit 读动态 / SF 完上报样本 / 日志加 effective\_limit
7. **main.go**：构造 ac / 注入 deps / 启动日志加字段
8. **proxy/handler\_concurrency\_test.go**：追加跨请求集成测试 → 跑通
9. **全量验证**：`go test -race ./...`，手动验证
10. **更新 HANDOFF.md**：P2 完成，加参数表 + 验证结论（如果用户要的话，不是必须）

***

## 8. 决策记录

| #  | 决策                                          | 选择                     | 理由                                                                             |
| -- | ------------------------------------------- | ---------------------- | ------------------------------------------------------------------------------ |
| D1 | 默认 enabled=false                            | 是                      | 先静默上线，用户熟悉静态配置后再手动开；保证 rollout 零风险                                             |
| D2 | 指标上报用 controller 内部（而不是 handler 打）          | controller 内           | 决策发生地就是指标更新地，避免状态不同步；handler 只负责样本输入                                           |
| D3 | 控制器放在 proxy 包（新建 adaptive.go）               | 不在 config / 不在 metrics | 它依赖 handler 语义（singleflight / errgroup），属于 proxy 层；config 只存配置不存运行逻辑           |
| D4 | P90 实现：切片排序                                 | 不用精确分位数算法              | window=20，O(N log N) 可以忽略；简单正确优先                                               |
| D5 | 样本窗口 = 简单追加 + 截断头                           | 不用环形缓冲索引               | 代码可读性更高；每次 append+slice 只移动 20 个元素，完全可接受                                       |
| D6 | current limit 读取用 mutex                     | 不引入 atomic             | 与窗口写入共用一把锁，避免错误的 lock-free 推理；读频率是「每请求 1 次」，写是「每 20 次 vision 调用 1 次」，mutex 无压力 |
| D7 | HandlerDeps.AdaptiveConcurrency 是指针（允许 nil） | 不是 embed struct        | nil 在 NewHandler 兜底为 disabled，方便现有 3 个 handler 测试零改动                           |

