# 交接文档 — blind-llm-eyes 视觉代理

> **最近更新**：2026-08-12（P1 配置化：concurrency_limit 可配置 + description_cap 降至 1000）
> **状态**：核心功能完成，性能优化已完成三轮（MiMo thinking 关闭 + errgroup 并行 + singleflight 去重），P1 配置化任务已完成，可端到端使用

---

## 0. 本次会话工作总结（50 轮对话）

### 起点
用户从上一个会话交接到此工作区，项目已有基础骨架（config/messages/proxy/vision/cache 五包），但 MiMo 视觉调用耗时过长（单图 31s），端到端体验差。

### 完成的工作（按时间线）

1. **MiMo 性能瓶颈定位** (commit `8a70576`)
   - 给 vision/client.go 加 httptrace 7 阶段日志
   - 发现 body_read_duration = 23.5s 是主要瓶颈
   - 根因：MiMo 默认 thinking 模式生成 2257 字符隐藏 reasoning_content

2. **MiMo thinking 模式关闭** (commit `a868455`)
   - OpenAI Chat Completions 端点不支持关闭 thinking
   - 切换到 Anthropic Messages API + `thinking.type: disabled`
   - body_read 23.5s → 4.2s (-82%)，reasoning_content_len = 0

3. **DeepSeek 上游日志插桩** (commit `6bf48b4`)
   - 给 proxy/handler.go 加 4 阶段 upstream 日志
   - 发现 DeepSeek TTFB 104ms，stream 8.2s（正常）

4. **errgroup 并行处理** (commit `9bfe130`)
   - handler.go 串行 for 循环改为 errgroup.Go 并行
   - SetLimit(4) 防止大量图片打爆 MiMo
   - 2 图端到端 39.7s → 19.8s (-50%)

5. **并行处理日志体系** (commit `c130070`)
   - 三层日志：errgroup / goroutine / singleflight
   - `outcome` 枚举：cache_hit / vision_success / vision_fail_open / vision_fail

6. **并发测试** (commit `b25ec2a`)
   - 5 图 × 2s mock 验证 4+1 批次行为
   - 前 4 个偏移 < 200ms，第 5 个偏移 ≥ 1900ms

7. **singleflight 去重** (commit `4cdae70`)
   - 同 hash in-flight 调用合并为 1 次
   - 关键修复：Cache.Put 移到 fn 内部，避免等待者比执行者更早 return 导致下批 miss
   - fn 用独立 ctx（context.Background + 120s），调用者取消不影响其他等待者
   - 3 个测试：stampede / cross-request / ctx isolation

8. **singleflight 耗时分解日志** (commit `9ec798c`)
   - `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms` 三字段
   - 执行者 sf_wait≈0，等待者 sf_wait=sf_total

9. **设计文档** (commit `b092fb6`)
   - CONCURRENCY_DESIGN.md：errgroup + concurrency_limit + singleflight 设计

10. **P1 配置化任务** (未提交)
    - `concurrency_limit` 从 handler.go 硬编码改为 config.yaml 读取（Config + HandlerDeps + main 全链路打通，NewHandler 兜底默认 4 保证向后兼容）
    - `description_cap` 默认值 2000 → 1000（thinking 已禁用，实测只生成 1000-1300 chars，旧注释「MiMo 是推理模型需要足够预算」已过时）
    - 新增测试 `TestHandler_ConcurrencyLimit_CustomValue`（limit=2 + 3 图 × 1s，验证配置真实生效，offsets `[14, 15, 1015]ms`）
    - `go test -race ./...` 全绿

### 当前状态
- **P1 配置化代码已完成但未提交**（5 文件变更 + 1 新测试，工作区有改动）
- **go test -race ./... 全绿**
- **服务可运行**：`.\blind-llm-eyes.exe` 启动后监听 127.0.0.1:8790
- **端到端验证通过**：2 图请求 19.8s，MiMo 阶段 14.2s，DeepSeek 阶段 5.5s
- **用户已手动把 config.yaml 的 `description_cap` 改为 1000**

### 下一步建议（按优先级）
| 优先级 | 任务 | 说明 |
|--------|------|------|
| ✅ P1 | ~~`concurrency_limit` 配置化~~ | **已完成**：Config + HandlerDeps + main 全链路打通，NewHandler 兜底默认 4 |
| ✅ P1 | ~~调小 `description_cap: 2000 → 1000`~~ | **已完成**：loader 默认值 + config.example.yaml 同步更新，用户 config.yaml 已改 |
| P2 | 自适应限流 | 根据 MiMo 响应时间动态调整并发度 |
| P3 | 多 vision provider | 抽象为 provider 池，支持故障转移 |

---

## 1. 项目一句话

本地轻量 Go 代理，架在 **Claude Code ↔ 上游 LLM** 之间：Claude Code 发请求含图片块 → 本地工具用 MiMo 视觉模型把图转成文字描述 → 替换图片块 → 转发给纯文本上游 DeepSeek → 流式响应原样透传。让用户**留在 DeepSeek、不切供应商、粘贴截图直接可用**地看图。

## 2. 当前架构

```
Claude Code ──POST /v1/messages──▶ blind-llm-eyes (127.0.0.1:8790)
                                    │
                                    ├─ 解析请求，提取 image blocks
                                    ├─ 查 LRU 缓存 (SHA-256 hash)
                                    │   ├─ 命中 → 直接用缓存的描述
                                    │   └─ miss → singleflight.Do(hash)
                                    │              ├─ 首个调用者 → MiMo vision API → 写缓存
                                    │              └─ 等待者 → 共享结果
                                    ├─ errgroup 并行处理多图 (concurrency_limit=4)
                                    ├─ 替换 image block → text block
                                    └─ 转发给 DeepSeek (anthropic 端点) ──▶ 流式响应透传
```

### 关键设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| MiMo API 端点 | Anthropic Messages API (`/anthropic`) | 支持 `thinking.type: disabled`，关闭思考模式后 body_read 从 23.5s → 4.2s |
| 多图处理 | errgroup 并行 + SetLimit(4) | 2 图场景端到端 39.7s → 19.8s (-50%) |
| 重复请求去重 | singleflight (进程级) | 同 hash in-flight 调用合并为 1 次 |
| 缓存写入时机 | singleflight fn 内部 | 避免等待者比执行者更早 return 导致下批 miss |
| ctx 隔离 | fn 用 context.Background + 120s | 调用者取消不影响其他等待者 |

## 3. 代码结构

```
blind-llm-eyes/
├── main.go                     # 入口 + graceful shutdown
├── config.yaml                 # 运行配置（gitignore，见 config.example.yaml）
├── config/
│   └── loader.go               # YAML 配置加载
├── messages/                   # Anthropic Messages API 请求解析/改写
│   ├── content.go              # ContentBlock 结构
│   ├── parse.go                # 请求解析
│   ├── rewrite.go              # 图片块替换为文字
│   └── validate.go             # 请求校验
├── vision/                     # MiMo 视觉客户端
│   ├── client.go               # Anthropic API 调用 + WebP 转换 + httptrace
│   └── provider.go             # VisionProvider 接口
├── proxy/
│   ├── handler.go              # 核心代理逻辑（errgroup + singleflight）
│   └── passthrough.go          # 流式响应透传
├── cache/
│   ├── lru.go                  # LRU 缓存
│   └── hash.go                 # SHA-256 内容哈希
├── logging/
│   └── logging.go              # 异步 JSON 日志 + request_id 传播
└── metrics/
    └── metrics.go              # Prometheus 指标
```

## 4. 性能优化历程

### 第一轮：MiMo thinking 模式关闭 (commit `a868455`)

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| body_read_duration | 23,500 ms | **4,153 ms** (-82%) |
| total_node_duration | 31,700 ms | **12,444 ms** (-61%) |
| reasoning_content_len | 2,257 | **0** |

将 MiMo 从 OpenAI Chat Completions 切换到 Anthropic Messages API，通过 `thinking.type: "disabled"` 关闭思考模式。

### 第二轮：errgroup 并行处理 (commit `9bfe130`)

| 场景 | 串行 | 并行 |
|------|------|------|
| 2 图端到端 | 39,689 ms | **19,754 ms** (-50%) |
| MiMo 阶段 | 31,300 ms | **14,200 ms** (-55%) |

`errgroup.SetLimit(4)` 限制单请求最多 4 个并发 vision 调用。

### 第三轮：singleflight 去重 (commit `4cdae70`)

| 场景 | 改前 | 改后 |
|------|------|------|
| 单请求 5 张相同图 | 4 次调用 | **1 次** |
| 10 个并发请求带同一张图 | 10 次调用 | **1 次** |

### 日志可观测性

三层日志体系（全部通过 `request_id` 关联）：
1. **errgroup 级**：`parallel_images_start` / `parallel_images_complete`
2. **goroutine 级**：`image_goroutine_start` / `image_goroutine_complete`（含 `outcome` 枚举）
3. **singleflight 级**：`singleflight_complete`（含 `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms` 耗时分解）

## 5. 配置

`config.example.yaml` 是模板，实际 `config.yaml` 被 gitignore。关键配置：

```yaml
listen: "127.0.0.1:8790"
upstream:
  base_url: "https://api.deepseek.com/anthropic"
  api_key: "sk-..."
vision:
  base_url: "https://api.xiaomimimo.com/anthropic"  # Anthropic 端点（非 /v1）
  api_key: "sk-..."
  model: "mimo-v2.5"
  timeout: "60s"
  large_image_timeout: "120s"
  description_cap: 1000
cache:
  max_entries: 500
concurrency_limit: 4
fail_open: true
log_level: "debug"
```

## 6. 使用方式

```powershell
# 构建
go build -o blind-llm-eyes.exe .

# 启动
.\blind-llm-eyes.exe

# 端点
# POST /v1/messages  — 代理接口
# GET  /healthz      — 健康检查
# GET  /metrics      — Prometheus 指标
```

CC Switch 配置：把 DeepSeek 供应商的 `ANTHROPIC_BASE_URL` 指向 `http://127.0.0.1:8790`（**不要用 CC Switch 代理模式**，会截断图片数据）。

## 7. 测试

```powershell
# 全量测试（含 race detector）
go test -race -count=1 ./...

# 单独跑并发/singleflight 测试
go test -race -v -run TestHandler_ ./proxy/
```

测试覆盖：
- `proxy/handler_test.go` — 基础代理逻辑
- `proxy/handler_concurrency_test.go` — errgroup 并行（5 图 × 2s mock，验证 4+1 批次）+ concurrency_limit 配置化（limit=2 + 3 图 × 1s，验证配置真实生效）
- `proxy/handler_singleflight_test.go` — singleflight 去重（stampede / cross-request / ctx 隔离）
- `vision/client_test.go` — MiMo Anthropic API 调用
- `messages/*_test.go` — 请求解析/改写/校验
- `cache/lru_test.go` — LRU 缓存
- `logging/logging_test.go` — 异步日志
- `metrics/metrics_test.go` — Prometheus 指标

## 8. 最近 6 个 commit

```
b092fb6 docs: add concurrency design doc for errgroup optimization
9ec798c feat(observability): add singleflight wait/exec duration breakdown logs
4cdae70 feat(proxy): add singleflight to dedup in-flight vision calls
b25ec2a test(proxy): add concurrency test for parallel image processing
c130070 feat(observability): add per-goroutine and errgroup-level logs
9bfe130 perf(proxy): parallelize image processing with errgroup
```

## 9. 已知限制 & 后续方向

| 优先级 | 方向 | 说明 |
|--------|------|------|
| ✅ 完成 | ~~`concurrency_limit` 配置化~~ | 已从 config.yaml 读取，NewHandler 兜底默认 4（commit 待提交） |
| ✅ 完成 | ~~调小 `description_cap`~~ | 默认值已降至 1000，config.example.yaml 同步更新，用户 config.yaml 已改 |
| P2 | 自适应限流 | 根据 MiMo 响应时间动态调整并发度 |
| P2 | MiMo TTFB ~8s | 服务端固定开销（视觉编码 + 预填充），客户端无法优化 |
| P3 | 多 vision provider 支持 | 当前硬编码 MiMo，可抽象为 provider 池 |

## 10. 环境事实

- **Go 版本**：go 1.22+（用了 loopvar 修复，但代码里仍显式 `i, blk := i, blk`）
- **Windows PowerShell**：环境变量用 `$env:VAR = "value"` 语法
- **CC Switch 代理模式会截断图片数据**（316→188 bytes），必须用 `ANTHROPIC_BASE_URL` 直连
- **MiMo 端点**：`https://api.xiaomimimo.com/anthropic`（Anthropic 格式），不是 `/v1`（OpenAI 格式）
- **真实 API key** 在 `config.yaml`（gitignore），不在代码里

## 11. 相关文档

| 文件 | 内容 |
|------|------|
| [CONCURRENCY_DESIGN.md](./CONCURRENCY_DESIGN.md) | errgroup 并发优化 + concurrency_limit 设计文档 |
| [vision-fallback-architecture.md](./vision-fallback-architecture.md) | 原始架构设计 v1 |
| [vision-fallback-notes.md](./vision-fallback-notes.md) | 决策时间线 + 调研记录 |
| [CONTEXT.md](./CONTEXT.md) | 项目上下文 |
