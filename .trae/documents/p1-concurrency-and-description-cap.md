# P1 任务实施计划：concurrency\_limit 配置化 + description\_cap 降至 1000

> 范围：仅 HANDOFF.md §9 列出的两个 P1 任务
> 模式：决策完备型计划，执行者无需再做选择题

***

## 1. 摘要

把 [proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go) 中硬编码的 `g.SetLimit(4)` 改为从 `config.yaml` 读取；同时把 `description_cap` 默认值从 2000 降到 1000（实测 MiMo 关闭 thinking 后只生成 1000–1300 chars，2000 是浪费）。

两个改动合在一起约 6 个文件、\~20 行净变更 + 1 个新测试。

***

## 2. 现状分析

### 2.1 concurrency\_limit 当前是硬编码

[proxy/handler.go#L154-L164](file:///d:/Code/new-api-contrib/proxy/handler.go#L154-L164):

```go
g := new(errgroup.Group)
g.SetLimit(4) // 限制并发，避免一次请求里大量图片打爆 MiMo

log.Info("parallel image processing started",
    ...
    "concurrency_limit", 4,  // 字面量 4，非配置驱动
    ...
)
```

* `HandlerDeps` struct ([proxy/handler.go#L29-L39](file:///d:/Code/new-api-contrib/proxy/handler.go#L29-L39)) 没有 `ConcurrencyLimit` 字段

* [config/loader.go](file:///d:/Code/new-api-contrib/config/loader.go) 的 `Config` struct 也没有对应字段

* [main.go#L60-L80](file:///d:/Code/new-api-contrib/main.go#L60-L80) 构造 `HandlerDeps` 时没传并发度

### 2.2 description\_cap 默认值偏高

[config/loader.go#L79-L81](file:///d:/Code/new-api-contrib/config/loader.go#L79-L81):

```go
if c.Vision.DescriptionCap <= 0 {
    c.Vision.DescriptionCap = 2000
}
```

[config.example.yaml#L10](file:///d:/Code/new-api-contrib/config.example.yaml#L10):

```yaml
description_cap: 2000                             # max_tokens，MiMo 是推理模型需要足够预算
```

注释已过时：MiMo 在 commit `a868455` 已切换到 Anthropic Messages API + `thinking.type: "disabled"`（见 [vision/client.go#L167-L169](file:///d:/Code/new-api-contrib/vision/client.go#L167-L169)），不再生成 `reasoning_content`。HANDOFF.md §4 实测：关闭 thinking 后 `reasoning_content_len = 0`，描述实际只生成 1000–1300 chars。`description_cap=2000` 多余预算不会加速响应，但可能让模型多生成冗长内容反而拖慢 body\_read。

### 2.3 已有测试不受影响（关键前提）

现有 3 个 handler 测试构造 `HandlerDeps` 时都**没有**设置 ConcurrencyLimit：

* [proxy/handler\_test.go#L53-L60](file:///d:/Code/new-api-contrib/proxy/handler_test.go#L53-L60) — `TestHandler_ImageReplaceAndCache`

* [proxy/handler\_concurrency\_test.go#L111-L118](file:///d:/Code/new-api-contrib/proxy/handler_concurrency_test.go#L111-L118) — `TestHandler_ParallelImageProcessing_5Images`

* `proxy/handler_singleflight_test.go`（3 个 singleflight 测试）

只要 `NewHandler` 在 `ConcurrencyLimit <= 0` 时回退到默认值 4（与 [proxy/handler.go#L43-L45](file:///d:/Code/new-api-contrib/proxy/handler.go#L43-L45) 现有 `WG` 默认值处理风格一致），这些测试无需改动即可继续通过。

***

## 3. 拟定变更

### 3.1 concurrency\_limit 配置化

#### 文件 1：[config/loader.go](file:///d:/Code/new-api-contrib/config/loader.go)

**变更 A** — `Config` struct 增加字段（与 `FailOpen` / `LogLevel` 同级，top-level，符合现有风格）：

```go
type Config struct {
    Listen   string      `yaml:"listen"`
    Upstream UpstreamCfg `yaml:"upstream"`
    Vision   VisionCfg   `yaml:"vision"`
    Cache    CacheCfg    `yaml:"cache"`
    FailOpen bool        `yaml:"fail_open"`
    LogLevel string      `yaml:"log_level"`
    ConcurrencyLimit int `yaml:"concurrency_limit"` // 单请求内并发 vision 调用上限
}
```

**变更 B** — `Load` 函数末尾加默认值（紧贴 `LogLevel` 默认值处理之后）：

```go
if c.ConcurrencyLimit <= 0 {
    c.ConcurrencyLimit = 4
}
```

**决策**：不加 `BLIND_CONCURRENCY_LIMIT` env 覆盖。现有 env 覆盖仅用于密钥/部署参数（`BLIND_VISION_API_KEY` / `BLIND_UPSTREAM_API_KEY` / `BLIND_LISTEN` / `BLIND_UPSTREAM_BASE_URL`），`concurrency_limit` 是调优参数属于 yaml 范畴，引入 env 路径会破坏约定分层。如后续确需运行时调参，再加不迟。

#### 文件 2：[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go)

**变更 C** — `HandlerDeps` struct 增加字段（[第 29-39 行](file:///d:/Code/new-api-contrib/proxy/handler.go#L29-L39)）：

```go
type HandlerDeps struct {
    UpstreamBaseURL     string
    UpstreamAPIKey      string
    VisionProvider      vision.VisionProvider
    Cache               *cache.LRU
    FailOpen            bool
    LargeImageThreshold int64
    ConcurrencyLimit    int // 单请求并发 vision 上限，<=0 时 NewHandler 兜底为 4
    Log                 *slog.Logger
    WG                  *sync.WaitGroup
    Metrics             *metrics.Metrics
}
```

**变更 D** — `NewHandler` 增加默认兜底（[第 42-50 行](file:///d:/Code/new-api-contrib/proxy/handler.go#L42-L50)，紧贴 `WG` 默认值之后）：

```go
func NewHandler(deps HandlerDeps) http.Handler {
    if deps.WG == nil {
        deps.WG = &sync.WaitGroup{}
    }
    if deps.ConcurrencyLimit <= 0 {
        deps.ConcurrencyLimit = 4
    }
    ...
}
```

**变更 E** — 替换 `g.SetLimit(4)` 和日志字面量（[第 154-164 行](file:///d:/Code/new-api-contrib/proxy/handler.go#L154-L164)）：

```go
g := new(errgroup.Group)
g.SetLimit(h.deps.ConcurrencyLimit)

log.Info("parallel image processing started",
    "stage", "parallel_images_start",
    "status", "info",
    "image_count", len(imgs),
    "concurrency_limit", h.deps.ConcurrencyLimit,
    "total_image_bytes", totalImageBytes,
)
```

#### 文件 3：[main.go](file:///d:/Code/new-api-contrib/main.go)

**变更 F** — `HandlerDeps` 构造时传入 `ConcurrencyLimit`（[第 60-80 行](file:///d:/Code/new-api-contrib/main.go#L60-L80)）：

```go
deps := proxy.HandlerDeps{
    UpstreamBaseURL: strings.TrimRight(cfg.Upstream.BaseURL, "/"),
    UpstreamAPIKey:  cfg.Upstream.APIKey,
    VisionProvider: vision.NewClient(...),
    Cache:               cache.NewLRU(cfg.Cache.MaxEntries),
    FailOpen:            cfg.FailOpen,
    LargeImageThreshold: cfg.Vision.LargeImageThreshold,
    ConcurrencyLimit:    cfg.ConcurrencyLimit,
    Log:                 logger,
    WG:                  &wg,
    Metrics:             m,
}
```

可选：在启动日志（[main.go#L38-L49](file:///d:/Code/new-api-contrib/main.go#L38-L49)）追加 `"concurrency_limit", cfg.ConcurrencyLimit` 字段，便于启动时确认配置已生效。

#### 文件 4：[config.example.yaml](file:///d:/Code/new-api-contrib/config.example.yaml)

**变更 G** — 在 `fail_open` 之前/之后追加新字段：

```yaml
concurrency_limit: 4                              # 单请求内并发 vision 调用上限（errgroup.SetLimit），覆盖典型 2-4 图场景
fail_open: true                                     # 视觉失败 → 替换为占位文字，不阻塞主链路
```

### 3.2 description\_cap 默认值 2000 → 1000

#### 文件 1：[config/loader.go](file:///d:/Code/new-api-contrib/config/loader.go)

**变更 H** — 修改默认值（[第 79-81 行](file:///d:/Code/new-api-contrib/config/loader.go#L79-L81)）：

```go
if c.Vision.DescriptionCap <= 0 {
    c.Vision.DescriptionCap = 1000
}
```

#### 文件 2：[config.example.yaml](file:///d:/Code/new-api-contrib/config.example.yaml)

**变更 I** — 修改值与注释（[第 10 行](file:///d:/Code/new-api-contrib/config.example.yaml#L10)）。原注释 `# max_tokens，MiMo 是推理模型需要足够预算` 已过时（thinking 已禁用）：

```yaml
description_cap: 1000                            # max_tokens；thinking 已禁用，实测生成 1000-1300 chars，1000 足够
```

### 3.3 新增测试

#### 文件：[proxy/handler\_concurrency\_test.go](file:///d:/Code/new-api-contrib/proxy/handler_concurrency_test.go)

**变更 J** — 在文件末尾追加一个新测试 `TestHandler_ConcurrencyLimit_CustomValue`，验证配置值真实生效（而非始终硬编码 4）：

```go
// TestHandler_ConcurrencyLimit_CustomValue 验证 HandlerDeps.ConcurrencyLimit
// 真实驱动 errgroup.SetLimit：设 limit=2 + 3 张图 × 1s，第 3 张应在第 1 批
// 完成后才开始（offset >= 900ms）。
func TestHandler_ConcurrencyLimit_CustomValue(t *testing.T) {
    var upstreamGot []byte
    up := fakeUpstream(t, &upstreamGot)
    defer up.Close()

    slow := newSlowVisionMock(1*time.Second, "SlowMockDesc")

    var logBuf bytes.Buffer
    logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

    deps := HandlerDeps{
        UpstreamBaseURL:     strings.TrimSuffix(up.URL, "/"),
        VisionProvider:      slow,
        Cache:               cache.NewLRU(10),
        FailOpen:            true,
        LargeImageThreshold: 1_000_000,
        ConcurrencyLimit:    2, // 关键：覆盖默认 4
        Log:                 logger,
    }
    h := NewHandler(deps)

    reqBody := buildNImageRequest(3)

    start := time.Now()
    rr := httptest.NewRecorder()
    req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
    req.Header.Set("Content-Type", "application/json")
    h.ServeHTTP(rr, req)
    elapsed := time.Since(start)

    if rr.Code != 200 {
        t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
    }

    // 3 张图 × 1s，limit=2 → 2 批：预期 ~2s（串行 3s）
    if elapsed > 3500*time.Millisecond {
        t.Errorf("elapsed = %v, want <3.5s (limit=2 not working?)", elapsed)
    }
    t.Logf("total elapsed: %v (3 images × 1s, concurrency_limit=2, expected ~2s)", elapsed)

    offsets := slow.offsets()
    if len(offsets) != 3 {
        t.Fatalf("vision calls = %d, want 3", len(offsets))
    }
    t.Logf("vision call start offsets (ms): %v", offsets)

    // 前 2 个并发启动
    for i := 0; i < 2; i++ {
        if offsets[i] > 200 {
            t.Errorf("vision call %d started at %dms, want <200ms", i, offsets[i])
        }
    }
    // 第 3 个应等第 1 批完成（>= 900ms）
    if offsets[2] < 900 {
        t.Errorf("vision call 2 started at %dms, want >=900ms (limit=2 should block)", offsets[2])
    }
    if offsets[2] > 1300 {
        t.Errorf("vision call 2 started too late: %dms, want <1300ms", offsets[2])
    }
}
```

**为什么是 limit=2 而不是 limit=4**：limit=2 + 3 张图能产生明显的「第 3 张必须等待」信号（offset ≥ 900ms），且 delay 用 1s 而非 2s 让测试更快（\~2s 完成）。如果用默认 4 + 5 图 × 2s 的现有测试，会被现有 `TestHandler_ParallelImageProcessing_5Images` 覆盖，新测试无法区分「默认 4」和「真实读到 4」。

***

## 4. 假设与决策

| #  | 决策                                                                                           | 理由                                                     |
| -- | -------------------------------------------------------------------------------------------- | ------------------------------------------------------ |
| D1 | `concurrency_limit` 放在 `Config` top-level，不新建 `proxy` 子节                                     | 与 `fail_open` / `log_level` 风格一致；目前 proxy 配置项少，不值得引入子节 |
| D2 | `NewHandler` 在 `ConcurrencyLimit <= 0` 时兜底为 4                                                | 让 3 个现有 handler 测试无需改动即可继续通过（它们都没设置该字段）                |
| D3 | 不加 `BLIND_CONCURRENCY_LIMIT` env 覆盖                                                          | 现有 env 覆盖仅用于密钥/部署参数；并发度是调优参数属 yaml 范畴                  |
| D4 | `description_cap` 默认值 2000 → 1000；同步更新 `config.example.yaml` 与注释                             | 保持模板与默认值一致；旧注释「MiMo 是推理模型需要足够预算」已过时（thinking 已禁用）      |
| D5 | 不修改用户真实 `config.yaml`（gitignored）                                                            | 用户控制；plan 仅在「验证步骤」提示用户手动降低                             |
| D6 | 不修 `config.example.yaml` 第 6 行的 `base_url: "https://api.xiaomimimo.com/v1"`（应为 `/anthropic`） | 超出 P1 范围；属预先存在的模板 bug，不在本次两个任务里                        |
| D7 | 新增 1 个测试用 `limit=2`（而非复用现有 limit=4 测试）                                                       | 用不同的 limit 值才能证明「配置真的被读取」，否则无法区分默认 4 与配置驱动 4           |

***

## 5. 验证步骤

执行者按顺序执行：

```powershell
# 1. 编译
go build ./...

# 2. 静态检查
go vet ./...

# 3. 全量测试（race detector）
go test -race -count=1 ./...

# 4. 单独跑新测试，确认 limit=2 行为
go test -race -v -run TestHandler_ConcurrencyLimit_CustomValue ./proxy/

# 5. 跑现有并发测试，确认默认 4 行为不退化
go test -race -v -run TestHandler_ParallelImageProcessing_5Images ./proxy/

# 6. 启动服务，确认日志中 concurrency_limit 来自 config
.\blind-llm-eyes.exe
# （另一终端）发一个带图请求，看启动日志和 parallel_images_start 日志中的
# concurrency_limit 字段应为 4（或 config.yaml 中设置的值）
```

**通过判据**：

* 步骤 1–5 全部 0 失败

* 启动日志包含 `concurrency_limit=4`（或用户在 config.yaml 自定义的值）

* `parallel_images_start` 日志的 `concurrency_limit` 字段与 config 一致

* 现有 `TestHandler_ParallelImageProcessing_5Images`（断言 5 图 limit=4 → 前 4 并发、第 5 等 ≥1900ms）继续通过

* 新增 `TestHandler_ConcurrencyLimit_CustomValue`（断言 3 图 limit=2 → 前 2 并发、第 3 等 ≥900ms）通过

**用户侧手动动作（不在代码变更内）**：

* 在 `config.yaml` 中把 `description_cap: 2000` 改为 `1000`（否则 loader 默认值变更对已有 config.yaml 不生效，因为 yaml 字段已显式存在）

* 可选：在 `config.yaml` 中新增 `concurrency_limit: 4`（不加也行，loader 默认 4）

* 重启服务后发 1 个带图请求，从 `/metrics` 或日志确认 `concurrency_limit` 字段值

***

## 6. 不在本次范围

按用户决策「仅 P1 两个任务」，以下明确**不做**：

* P2 自适应限流（AIMD / token bucket 动态调整并发度）

* P3 多 vision provider 池 + 故障转移

* `config.example.yaml` 中 `vision.base_url` 仍指向过时的 `/v1`（OpenAI 端点），实际代码用 `/anthropic` — 预先存在的模板 bug，本次不修

* HANDOFF.md 文档更新 — 完成后由用户决定是否更新「下一步建议」表把 P1 标记为已完成

