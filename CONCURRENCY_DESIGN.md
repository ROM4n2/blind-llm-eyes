# errgroup 并发优化与 concurrency_limit 机制

## 1. 背景

`blind-llm-eyes` 在处理 `/v1/messages` 请求时，需要把消息体里的图片块送给 MiMo 视觉模型生成文字描述，再把改写后的请求转发给 DeepSeek 上游。

优化前，[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go) 用普通 `for` 循环逐张处理图片：

```go
for i, blk := range imgs {
    // 查缓存 → 未命中调 vision → 改写 block
}
```

MiMo 单次调用约 12–14s（已关闭 thinking 模式），多图请求会串行累加：

| 图片数 | 串行总耗时（MiMo 阶段） |
|--------|------------------------|
| 1      | ~14s                   |
| 2      | ~31s（18.9s + 12.4s）  |
| 5      | ~70s（理论值）         |

DeepSeek 上游仅占 ~8s，串行 MiMo 是绝对瓶颈。

## 2. 方案

用 [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) 把 for 循环改为 goroutine 并发，每张图在独立 goroutine 里跑完「查缓存 → 调 vision → 改写 block」全流程。

### 2.1 核心代码

位置：[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go#L148-L315)

```go
g, gctx := errgroup.WithContext(r.Context())
g.SetLimit(4) // concurrency_limit

for i, blk := range imgs {
    i, blk := i, blk
    g.Go(func() error {
        // 查缓存 → 未命中调 vision → 改写 block
        // 失败时根据 fail_open 决定返回 error 或 placeholder
    })
}
if err := g.Wait(); err != nil {
    // fail_open=false 时返回 502
}
```

### 2.2 并发安全依据

- `FindImageBlocks` 返回 `[]*ContentBlock`（指向 `Request` 内部 block 的指针）
- 每个 goroutine 写不同的 `*ContentBlock`，无写写冲突
- 计数器 `rewritten / cached / failed` 是 `atomic.Int64`
- `cache.LRU` 内部自带锁
- `go test -race ./...` 全绿

## 3. concurrency_limit = 4 的设计依据

### 3.1 为什么需要上限

不设上限时，一次请求里 20 张图会瞬间起 20 个 goroutine，同时打 MiMo API：
- MiMo 服务端可能有并发限制（429/超时）
- 大量并发会争抢本地 HTTP 连接池、内存
- 单请求资源占用爆炸，影响其他请求的公平性

### 3.2 为什么是 4

| 候选值 | 评估 |
|--------|------|
| 1      | 等于串行，无意义 |
| 2–3    | 改善有限，2 图请求仍无法完全重叠 |
| **4**  | 兼顾常见场景（用户粘贴 2–4 张图）与 MiMo 承受能力 |
| 8+     | 过度并发，MiMo 端可能限流；本地收益递减（受 TTFB ~8s 限制） |

4 是经验值：覆盖典型多图请求（2–4 张），同时避免对 MiMo 形成过大压力。通过 `g.SetLimit()` 实现，超出数量的 goroutine 会在 errgroup 内部的信号量上阻塞等待。

### 3.3 批次行为示例

5 张图、每张 2s、limit=4：

```
t=0ms      4 个 goroutine 同时启动（批次 1）
t=2000ms   批次 1 完成，第 5 个 goroutine 启动（批次 2）
t=4000ms   批次 2 完成
总耗时 ≈ 4s（串行需 10s）
```

测试验证：[proxy/handler_concurrency_test.go](file:///d:/Code/new-api-contrib/proxy/handler_concurrency_test.go) 中 `TestHandler_ParallelImageProcessing_5Images` 断言前 4 个调用偏移 < 200ms，第 5 个调用偏移 ≥ 1900ms。

## 4. 错误处理与 context 取消

### 4.1 errgroup.WithContext 的取消语义

```go
g, gctx := errgroup.WithContext(r.Context())
```

`gctx` 是 `r.Context()` 的派生 context。任一 goroutine 返回 error 时：
1. errgroup 取消 `gctx`
2. 其他 in-flight goroutine 的 `visionCtx`（用 `gctx` 派生）收到 cancel
3. MiMo HTTP 请求中断，避免无谓等待

### 4.2 fail_open 策略

| 场景 | goroutine 行为 | errgroup 行为 |
|------|----------------|---------------|
| vision 成功 | 写缓存 + 改写 block + return nil | 继续等待其他 |
| vision 失败 + `fail_open=true` | placeholder 改写 + return nil | 继续等待其他 |
| vision 失败 + `fail_open=false` | return error | 取消其他 in-flight → Wait 返回 error → 502 |

`fail_open=true`（默认）保证单张图失败不影响整批请求；`fail_open=false` 快速失败，避免慢请求拖累。

## 5. 性能数据

### 5.1 真实端到端（testdata/makoto_01.png + makoto_02.png，冷缓存）

| 场景 | 总耗时 | MiMo 阶段 | DeepSeek 阶段 |
|------|--------|-----------|---------------|
| 串行 | 39,689 ms | 31,300 ms（18.9s + 12.4s） | 8,400 ms |
| **并行** | **19,754 ms** | **14,200 ms**（max(13.1, 14.2)） | 5,500 ms |
| 加速 | **-50%**（2.0x） | -55%（2.2x） | -35% |

### 5.2 Mock 端到端（5 图 × 2s delay）

| 场景 | 总耗时 |
|------|--------|
| 串行理论值 | 10,000 ms |
| **并行实测** | **4,020 ms** |
| 加速 | **2.5x** |

## 6. 可观测性

在并行段落加了三层日志，全部通过 `request_id` 关联，便于排查超时：

| 层级 | stage | 关键字段 |
|------|-------|----------|
| errgroup | `parallel_images_start` | `image_count, concurrency_limit, total_image_bytes` |
| errgroup | `parallel_images_complete` | `duration_ms, rewritten, cached, failed` |
| goroutine | `image_goroutine_start` | `index, total_images` |
| goroutine | `image_goroutine_complete` | `index, outcome, duration_ms, err` |
| vision | `image block processed successfully` | `vision_duration_ms, desc_len` |

`outcome` 枚举：`cache_hit` / `vision_success` / `vision_fail_open` / `vision_fail`

实现技巧：用 `defer` + 局部 `outcome` 变量统一记录 goroutine 结束日志，避免在 4 个 return 分支重复写日志代码。

排查超时流程：
1. 按 `request_id` 过滤日志
2. 找 `parallel_images_complete` 看 `duration_ms` 是否异常
3. 找 `image_goroutine_complete` 按 `duration_ms` 排序，定位最慢的 `index`
4. 看该 index 的 `outcome` 判断是 vision 慢还是缓存/失败

## 7. 提交记录

| commit | 说明 |
|--------|------|
| `9bfe130` | perf(proxy): parallelize image processing with errgroup |
| `c130070` | feat(observability): add per-goroutine and errgroup-level logs |
| `b25ec2a` | test(proxy): add concurrency test for parallel image processing |

## 8. 后续可优化方向

- **concurrency_limit 配置化**：当前硬编码为 4，可改为从 `config.yaml` 读取
- **自适应限流**：根据 MiMo 响应时间动态调整并发度（token bucket）
- **cache stampede 防护**：同 hash 并发请求只调一次 vision，其他等结果（singleflight）
