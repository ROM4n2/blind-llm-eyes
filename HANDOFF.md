# 交接文档 — blind-llm-eyes 视觉代理

> **最近更新**：2026-08-13（代码审查 11 个问题修复 + 并发压力测试）
> **状态**：MVP → P1 → P2 → P3 全部完成 + 代码审查修复。多 Vision Provider 池支持优先级故障转移 + 三态熔断器 + OpenAI 兼容客户端，端到端验证通过。代码审查发现 5 严重 + 4 中等 + 2 低优先级共 11 个问题全部修复，含 singleflight 数据竞争、HTTP 超时、缓存污染、Header 泄露、base64 解码冗余、递归深度限制等。跨请求并发压力测试通过 `-race` 检测。安全修复：API key 泄露清理 + pre-commit hook 防护。本地领先 origin/master 5 个 commit（待推送）

---

## 0. 本次会话工作总结

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

10. **P1 配置化任务** (commit `ac5b1f9`, `69e8639`)
    - `concurrency_limit` 从 handler.go 硬编码改为 config.yaml 读取（Config + HandlerDeps + main 全链路打通，NewHandler 兜底默认 4 保证向后兼容）
    - `description_cap` 默认值 2000 → 1000（thinking 已禁用，实测只生成 1000-1300 chars，旧注释「MiMo 是推理模型需要足够预算」已过时）
    - 新增测试 `TestHandler_ConcurrencyLimit_CustomValue`（limit=2 + 3 图 × 1s，验证配置真实生效，offsets `[14, 15, 1015]ms`）
    - `go test -race ./...` 全绿

11. **P2 自适应限流 AIMD 控制器** (commit `f6f58dc`, `e60e098`)
    - 新增 `proxy/adaptive.go`：基于 P90 反馈的 AIMD 控制器（加性增 +1 / 乘性减 ×0.75）
    - 滚动窗口（sample_window=20）+ cooldown（3s）+ 滞回区（8s ≤ P90 ≤ 15s 不调整）
    - 仅 singleflight executor（`shared=false`）上报样本，避免 N 等待者放大 1 次 MiMo 调用
    - 3 个 Prometheus 指标：`current` / `adjustments_total{direction}` / `vision_p90_seconds`
    - 5 个单元测试 + 1 个跨请求集成测试 + 2 个端到端验证脚本（test-adaptive-prod.py / test-adaptive.ps1）
    - 生产配置三阶段验证全 PASS：Phase 1 (3s) limit 4→9 / Phase 2 (11s) 滞回稳住 10 / Phase 3 (16s) limit 10→1

12. **生产冒烟测试** (commit `b651276`)
    - 20 次真实 MiMo vision 调用，成功率 100%，零错误
    - P90 = 11837ms (11.8s)，落在滞回区 [8s, 15s]
    - AIMD 首次评估决策正确：`no change (hysteresis band)`，limit 保持 4
    - MiMo 延迟范围 4.25s – 20.60s（波动 4.8 倍），DeepSeek 仅占 16.5%
    - 完整测试报告见 `SMOKE_TEST_REPORT.md`

13. **配置参数调优** (commit `ccd5249`)
    - 基于冒烟测试 20 样本数据调整默认参数：
      - `concurrency_limit`: 4 → 6（MiMo 均值 7.7s < fast_threshold 8s，有升并发空间）
      - `max_limit`: 16 → 12（MiMo 最差 20.6s，12 路已足够避免压垮）
      - `sample_window`: 20 → 10（评估周期 4min → 2min，响应更快）
      - `cooldown_ms`: 3000 → 2000（适应 MiMo 4-20s 高波动）
    - 同步更新 config.example.yaml / config/loader.go / proxy/adaptive.go 兜底默认值
    - `go build` / `go vet` / `go test -race ./proxy/` 全绿

14. **P3 多 Provider 池 + 熔断故障转移** (commit `c14d1a5`)
    - 新增 `vision/pool.go`：Provider 池实现 `VisionProvider` 接口，按优先级遍历，跳过熔断器开启的 provider，失败时自动故障转移
    - 新增 `vision/circuit_breaker.go`：三态熔断器（Closed → Open → Half-open），线程安全，半开态单试探请求保护
    - 新增 `vision/openai_client.go`：通用 OpenAI 兼容客户端（`/v1/chat/completions` + `image_url` 格式）
    - `config/loader.go` 新增 `ProviderCfg` + `CircuitBreakerCfg`，向后兼容（`vision_providers` 缺省 = 单 provider 模式，行为不变）
    - `metrics/metrics.go` 新增 4 个 per-provider 指标：`provider_calls_total{provider,result}` / `provider_duration_seconds{provider}` / `circuit_breaker_state{provider}` / `failover_events_total`
    - `main.go` 条件构造：`vision_providers` 存在时构建 Pool，否则单 provider 直连
    - **handler.go 零改动**：Pool 透明注入 `HandlerDeps.VisionProvider`，singleflight / errgroup / AIMD 全部不变
    - 19 个新测试全部通过（`go test -race`）

15. **P3 详细日志埋点** (commit `c8dd7b9`)
    - `CircuitBreaker.Stats()` 方法暴露完整状态快照（State / ConsecutiveFails / FailureThreshold / OpenedAgo / HalfOpenInFlight）
    - Pool.DescribeImage 新增 9 个日志 stage：`pool_start` / `provider_call_start` / `cb_transition` / `cb_opened` / `cb_recovered` / `provider_skipped`（增强）/ `provider_failover`（增强）/ `pool_complete` / `pool_exhausted`（增强）
    - 熔断器状态转换全链路可追踪：Closed→Open / Open→HalfOpen / HalfOpen→Closed / HalfOpen→Open

16. **安全修复：API key 泄露清理 + 防护** (commit `ecdcb8c`, `c8dd7b9`)
    - `smoke-fill-samples.py` 硬编码 DeepSeek key → 改为 `os.environ.get("BLIND_LLM_EYES_API_KEY")`
    - `.githooks/pre-commit`：扫描暂存文件中 `sk-[A-Za-z0-9]{20,}` 模式，匹配则阻止提交
    - `git config core.hooksPath .githooks` 已配置
    - `.gitignore` 增强：`config-*-test.yaml` + `.trae/` IDE 产物
    - **git 历史清理**：`git filter-repo --replace-text` 将 40 个 commit 中的泄露 key 替换为 `REDACTED_USE_ENV_VAR`，已 force push

17. **Bug 修复：system message 规范化** (commit `14858c0`)
    - Claude Code 发送 `role:system` 消息在 `messages` 数组中（非顶层 `system` 字段），导致 `Validate()` 拒绝 → 400 错误
    - 新增 `messages.NormalizeSystemMessages()`：提取 `role:system` 条目合并到顶层 `system` 字段
    - handler 在 `Validate()` 前调用，确保对话正常工作

18. **P3 端到端验证**
    - 使用 `config-failover-test.yaml`（primary 用假 key → 401 快速失败，fallback 用真实 MiMo key）
    - 5 张不同颜色 8x8 PNG 验证完整链路：
      - Req 1-3：primary 失败 → 故障转移到 fallback → 成功
      - 第 3 次失败后熔断器开启（threshold=3）
      - Req 4-5：primary 被跳过（circuit open），直接命中 fallback
    - Metrics 验证：`circuit_breaker_state{provider="mimo-broken"}=1`(OPEN) / `failover_events_total=3` / `provider_calls_total{result="skipped"}=2`

19. **tool_result 嵌套 image 支持** (commit `4fa5821` 前置)
    - `messages/content.go` MarshalJSON 合并 raw 策略：tool_result 保留原始字段，仅覆盖 content
    - `messages/parse.go` FindImageBlocks 递归查找 tool_result 内嵌 image 块
    - `messages/rewrite.go` ReplaceImageWithDescription 先 Marshal 再赋值，保证状态一致
    - `proxy/handler.go` 请求体大小上限 413（MaxBodyBytes 默认 20MB）
    - Validate 放行未知 content block 类型（删除白名单）

20. **核心流程日志增强** (commit `4fa5821` 前置)
    - 9 个 stage 日志：request_start / body_read_complete / json_parse_complete / system_normalize_complete / validate_complete / find_images_complete / image_cache_hit / image_cache_miss / remarshal_complete
    - 每条含 request_id 关联，支持 grep 按 stage 过滤

21. **代码审查：5 个严重问题修复** (commit `4fa5821`)
    - **singleflight 数据竞争**：fnStart/fnEnd 在 executor 写入、waiter 读取导致竞态 → 新增 `visionResult` 结构体封装返回值，耗时数据与业务数据一起通过返回值传递
    - **上游 HTTP 无超时**：`http.DefaultClient` 无超时，上游挂起时代理永久阻塞 → 自定义 `http.Client`（30s 连接超时 + 90s 空闲超时 + 连接池）
    - **ReplaceImageWithDescription 静默丢失 Marshal 错误**：先改 blk.Type 再 Marshal，失败时状态不一致 → 先 Marshal 成功再修改字段
    - **Hash 失败污染缓存**：空 hash key 导致所有 hash 失败的图片去重到同一 key → hash 失败时提前返回，不进入缓存和 singleflight
    - **Authorization Header 泄露**：客户端 Authorization 被转发到上游 → 显式过滤 Authorization / Proxy-Authorization / Cookie

22. **代码审查：4 个中等问题修复** (commit `467f6c7`)
    - **三次 base64 解码冗余**：统计/hash/goroutine 各解码一次 → 新增 `HashFromRawBytes`，预解码所有图片存入 `decodedImages` 复用
    - **truncate 内存分配**：`truncate(string(rawBody),200)` 将整个请求体转为字符串 → 新增 `truncateBytes` 直接操作字节切片，仅转换前 N 字节
    - **collectImageBlocks 无界递归**：恶意构造深度嵌套可栈溢出 → 新增 `maxCollectDepth=16` 限制
    - **MarshalJSON 空 Content 覆盖 null**：空 Content 数组被 marshal 为 `null` 覆盖原始 JSON → 仅 `len(Content)>0` 时才覆盖

23. **代码审查：2 个低优先级问题修复 + 并发压力测试** (commit `c0a2fa7`)
    - **合并 message 遍历**：toolResultCount 合并到统计遍历中，消除一次完整 req.Messages 遍历
    - **NewHandler nil 检查补全**：新增 `UpstreamBaseURL` 空字符串 panic + `LargeImageThreshold` 默认 1MB
    - **跨请求并发压力测试**：20 goroutine 混合 3 种图片组合（含嵌套 tool_result），vision 调用从潜在 40 次去重到 3 次；AdaptiveConcurrency 100 次并发 RecordSample 无竞态

### 当前状态
- **MVP → P1 → P2 → P3 全部完成 + 代码审查 11 个问题全部修复**
- **go test -race ./... 全绿**，`go build` / `go vet` 零警告
- **服务可运行**：`go run . -config config.yaml` 启动后监听 127.0.0.1:8790
- **多 Provider 池已验证**：故障转移 + 熔断器 + 跳过逻辑端到端通过
- **安全防护已就位**：pre-commit hook 阻止 key 泄露，git 历史已清理
- **并发安全已验证**：跨请求压力测试（20 goroutine + 100 并发采样）`-race` 无报告
- **本地领先 origin/master 5 个 commit**（待推送）

### 下一步建议（按优先级）
| 优先级 | 任务 | 说明 |
|--------|------|------|
| ✅ P1 | ~~`concurrency_limit` 配置化~~ | **已完成**：Config + HandlerDeps + main 全链路打通 |
| ✅ P1 | ~~调小 `description_cap: 2000 → 1000`~~ | **已完成**：loader 默认值 + config.example.yaml 同步更新 |
| ✅ P2 | ~~自适应限流~~ | **已完成**：AIMD + 单测 + 集成测试 + 端到端验证 + 冒烟测试 + 配置调优 |
| ✅ P3 | ~~多 vision provider 池~~ | **已完成**：Provider 池 + 熔断器 + OpenAI 客户端 + 故障转移验证 |
| ✅ | ~~安全修复~~ | **已完成**：key 泄露清理 + pre-commit hook + git 历史清理 |
| ✅ | ~~system message 修复~~ | **已完成**：NormalizeSystemMessages 修复 Claude Code 400 错误 |
| ✅ | ~~tool_result 嵌套 image~~ | **已完成**：递归查找 + 合并 raw 序列化 + round-trip 测试 |
| ✅ | ~~请求体大小上限~~ | **已完成**：MaxBodyBytes 413 + 配置化 |
| ✅ | ~~核心流程日志增强~~ | **已完成**：9 个 stage 日志 + request_id 关联 |
| ✅ | ~~代码审查 11 个问题~~ | **已完成**：5 严重 + 4 中等 + 2 低优先级全部修复 |
| — | 推送到远程 | 本地领先 origin/master 5 个 commit |
| — | MiMo TTFB ~8s | 服务端固定开销（视觉编码 + 预填充），客户端无法优化 |
| P4 | 主动健康检查 | 定期 ping provider 检测恢复，当前仅被动熔断（reset_timeout 后半开试探） |
| P4 | 加权负载均衡 | 当前仅 priority failover，可扩展为 weighted round-robin |
| **P5** | **上下文感知描述** | **下一迭代规划，详见第 12 节** |

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
                                    │              ├─ 首个调用者 → Pool.DescribeImage → 写缓存
                                    │              └─ 等待者 → 共享结果
                                    ├─ errgroup 并行处理多图 (concurrency_limit=6, adaptive [1,12])
                                    ├─ 替换 image block → text block
                                    └─ 转发给 DeepSeek (anthropic 端点) ──▶ 流式响应透传

Pool 内部（vision/pool.go）:
  ┌─ Provider[0] (priority=1, e.g. MiMo)  ← CircuitBreaker: Closed/Open/Half-open
  ├─ Provider[1] (priority=2, e.g. GPT-4o) ← CircuitBreaker: Closed/Open/Half-open
  └─ ...按优先级遍历，跳过熔断器开启的，失败时自动故障转移
```

### 关键设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| MiMo API 端点 | Anthropic Messages API (`/anthropic`) | 支持 `thinking.type: disabled`，关闭思考模式后 body_read 从 23.5s → 4.2s |
| 多图处理 | errgroup 并行 + SetLimit(4) | 2 图场景端到端 39.7s → 19.8s (-50%) |
| 重复请求去重 | singleflight (进程级) | 同 hash in-flight 调用合并为 1 次 |
| 缓存写入时机 | singleflight fn 内部 | 避免等待者比执行者更早 return 导致下批 miss |
| ctx 隔离 | fn 用 context.Background + 120s | 调用者取消不影响其他等待者 |
| 自适应限流 | AIMD + P90 反馈 | 静态并发度无法应对 MiMo 延迟波动（8s~38s），动态调整防打爆 / 提吞吐 |
| Provider 池 | Pool 实现 VisionProvider 接口 | handler.go 零改动，singleflight/errgroup/AIMD 全部不变，Pool 透明注入 |
| 熔断器 | 三态（Closed/Open/Half-open） | 连续失败达阈值 → 开启跳过；reset_timeout 后半开试探；成功恢复 / 失败重开 |
| 故障转移策略 | 优先级 failover | 简单可靠，backup 仅在 primary 故障时启用；加权 LB 留作 P4 |
| 配置兼容 | `vision_providers` 缺省 = 单 provider | 向后兼容，现有 config.yaml 无需改动 |
| singleflight 返回值 | `visionResult` 结构体封装 | 消除 executor/waiter 间 fnStart/fnEnd 数据竞争 |
| 上游 HTTP 客户端 | 自定义 `http.Client` + 30s 连接超时 | 防止上游挂起时代理永久阻塞；不设整体超时以支持 SSE 流式 |
| base64 解码 | 预解码一次复用 `decodedImages` | 消除统计/hash/goroutine 三次冗余解码 |
| 递归深度限制 | `maxCollectDepth=16` | 防止恶意深度嵌套 tool_result 导致栈溢出 |
| 日志预览截断 | `truncateBytes` 直接操作字节 | 避免 `string(rawBody)` 分配完整请求体内存 |
| 请求头过滤 | 显式剥离 Authorization/Cookie | 防止客户端凭据泄露到上游（当 UpstreamAPIKey 已配置时） |

## 3. 代码结构

```
blind-llm-eyes/
├── main.go                     # 入口 + graceful shutdown + Pool/单 provider 条件构造
├── config.yaml                 # 运行配置（gitignore，见 config.example.yaml）
├── .githooks/pre-commit        # API key 泄露检测（sk-[A-Za-z0-9]{20,}）
├── config/
│   └── loader.go               # YAML 配置加载 + ProviderCfg + CircuitBreakerCfg
├── messages/                   # Anthropic Messages API 请求解析/改写
│   ├── content.go              # ContentBlock 结构
│   ├── parse.go                # 请求解析
│   ├── rewrite.go              # 图片块替换为文字 + NormalizeSystemMessages
│   ├── normalize_test.go       # system message 规范化测试
│   └── validate.go             # 请求校验
├── vision/                     # Vision Provider 层
│   ├── provider.go             # VisionProvider 接口
│   ├── client.go               # MiMo 客户端（Anthropic API + WebP + httptrace）
│   ├── openai_client.go        # 通用 OpenAI 兼容客户端（/v1/chat/completions）
│   ├── pool.go                 # 多 Provider 池（优先级 failover + 详细日志）
│   ├── circuit_breaker.go      # 三态熔断器 + Stats() 状态快照
│   ├── pool_test.go            # 池测试（优先级/故障转移/熔断/并发）
│   └── circuit_breaker_test.go # 熔断器测试（状态转换/半开/并发）
├── proxy/
│   ├── handler.go              # 核心代理逻辑（errgroup + singleflight + AIMD + 安全过滤）
│   ├── adaptive.go             # AIMD 自适应限流控制器
│   ├── passthrough.go          # 流式响应透传
│   ├── handler_integration_fix_test.go  # 5 个严重问题修复集成测试
│   ├── handler_medium_fix_test.go       # 4 个中等问题修复 + 端到端嵌套测试
│   └── handler_race_stress_test.go      # 跨请求并发压力测试（-race）
├── cache/
│   ├── lru.go                  # LRU 缓存
│   └── hash.go                 # SHA-256 内容哈希
├── logging/
│   └── logging.go              # 异步 JSON 日志 + request_id 传播
└── metrics/
    └── metrics.go              # Prometheus 指标（含 per-provider 指标）
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

### 第四轮：AIMD 自适应限流 (commit `f6f58dc`)

| 场景 | 静态 limit=4 | 自适应 [1, 16] |
|------|------|------|
| MiMo 快 (P90=3s) | 限并发，浪费吞吐 | **limit 自动升到 9**（+5） |
| MiMo 正常 (P90=11s) | 可能打爆 | **滞回区稳住 10**（不降） |
| MiMo 慢 (P90=16s) | 4 路并发全堆 MiMo | **limit 自动降到 1**（×0.75^5） |

控制器：`proxy/adaptive.go`，基于 singleflight executor 的 `fn_exec_ms` 滚动窗口 P90，AIMD 决策（加性增 +1 / 乘性减 ×0.75），滞回区防抖动，cooldown 3s 防震荡。

### 日志可观测性

三层日志体系（全部通过 `request_id` 关联）：
1. **errgroup 级**：`parallel_images_start` / `parallel_images_complete`
2. **goroutine 级**：`image_goroutine_start` / `image_goroutine_complete`（含 `outcome` 枚举）
3. **singleflight 级**：`singleflight_complete`（含 `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms` 耗时分解）

## 5. 配置

`config.example.yaml` 是模板，实际 `config.yaml` 被 gitignore。关键配置：

### 单 Provider 模式（向后兼容）

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
concurrency_limit: 6
fail_open: true
log_level: "debug"
adaptive_concurrency:
  enabled: true                # 默认 false，开启后根据 MiMo 延迟动态调整 concurrency_limit
  min_limit: 1
  max_limit: 12                # 基于冒烟测试：MiMo 最差 20.6s，12 路已足够
  fast_threshold_ms: 8000      # P90 < 8s → 加性增 +1
  slow_threshold_ms: 15000     # P90 > 15s → 乘性减 ×0.75
  sample_window: 10            # 10 样本约 2 分钟评一次
  cooldown_ms: 2000            # 两次调整最小间隔
  increase_step: 1
  decrease_ratio: 0.75
  error_threshold: 0.10        # 错误率 > 10% → 触发降并发
```

### 多 Provider 池模式（P3 新增，opt-in）

```yaml
# vision: 块在 vision_providers 存在时被忽略
vision_providers:
  - name: "mimo"                    # 标识符（用于日志/metrics）
    type: "mimo"                     # "mimo" = Anthropic Messages API
    priority: 1                      # 数值越小越先尝试
    base_url: "https://api.xiaomimimo.com/anthropic"
    api_key: "sk-..."
    model: "mimo-v2.5"
    timeout: "60s"
    description_cap: 1000
    circuit_breaker:
      failure_threshold: 5           # 连续失败 5 次 → 熔断
      reset_timeout: "30s"           # 30s 后半开试探

  - name: "openai-fallback"
    type: "openai_compatible"        # "openai_compatible" = /v1/chat/completions
    priority: 2
    base_url: "https://api.openai.com/v1"
    api_key: "sk-..."
    model: "gpt-4o"
    timeout: "30s"
    description_cap: 1000
    circuit_breaker:
      failure_threshold: 3
      reset_timeout: "30s"
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
- `proxy/handler_concurrency_test.go` — errgroup 并行（5 图 × 2s mock，验证 4+1 批次）+ concurrency_limit 配置化 + 自适应限流跨请求集成测试
- `proxy/handler_singleflight_test.go` — singleflight 去重（stampede / cross-request / ctx 隔离）
- `proxy/handler_integration_fix_test.go` — 5 个严重问题修复集成测试（race / timeout / marshal / cache pollution / header leak）
- `proxy/handler_medium_fix_test.go` — 4 个中等问题修复 + 嵌套 tool_result 端到端测试
- `proxy/handler_race_stress_test.go` — 跨请求并发压力测试（20 goroutine 混合图片 + AdaptiveConcurrency 100 并发采样）
- `proxy/adaptive_test.go` — AIMD 控制器单元测试（increase / decrease / error-trigger / hysteresis / disabled-is-static）
- `vision/client_test.go` — MiMo Anthropic API 调用
- `messages/*_test.go` — 请求解析/改写/校验（含嵌套 tool_result round-trip 测试）
- `cache/lru_test.go` — LRU 缓存
- `logging/logging_test.go` — 异步日志
- `metrics/metrics_test.go` — Prometheus 指标
- `test-adaptive-prod.py` / `test-adaptive.ps1` — 自适应限流端到端验证脚本（mock MiMo + 三阶段 AIMD 验证）
- `smoke-fill-samples.py` — 生产冒烟测试脚本（9 请求 × 2 唯一图 = 18 样本填窗口）
- `SMOKE_TEST_REPORT.md` — 生产冒烟测试报告（20 样本完整延迟数据 + AIMD 决策日志）
- `vision/pool_test.go` — Provider 池测试（优先级排序 / 故障转移 / 熔断跳过 / 全部失败 / 并发安全）
- `vision/circuit_breaker_test.go` — 熔断器测试（状态转换 / 半开单试探 / reset_timeout / 并发安全）
- `messages/normalize_test.go` — system message 规范化测试（5 场景：无 system / 单条 / 多条 / 空内容 / JSON 往返）
- `test-failover.py` — P3 故障转移端到端验证脚本（5 张不同图片验证熔断器开启→跳过链路）

## 8. 最近 commit

```
c0a2fa7 fix(proxy): merge message traversals and add missing nil checks in NewHandler
467f6c7 fix: resolve 4 medium-priority issues in image processing pipeline
4fa5821 fix(proxy): resolve 5 critical bugs in image processing pipeline
206f1d6 feat(messages): add support for tool_result content type
6b7dde0 feat(vision): add circuit breaker stats and improve failover logging
c8dd7b9 chore(pre-commit): update api key detection pattern to cover more services
c14d1a5 feat(vision): multi-provider pool with circuit breaker failover
14858c0 fix(proxy): normalize system messages before validation
```

> 注：commit hash 在 git filter-repo 清理后已变更，旧 hash 不再有效。

## 9. 已知限制 & 后续方向

| 优先级 | 方向 | 说明 |
|--------|------|------|
| ✅ 完成 | ~~P1 配置化~~ | concurrency_limit + description_cap 从 config.yaml 读取 |
| ✅ 完成 | ~~P2 自适应限流~~ | AIMD 控制器 + 单测 + 集成测试 + 端到端验证 + 冒烟测试 + 配置调优 |
| ✅ 完成 | ~~P3 多 Provider 池~~ | Provider 池 + 熔断器 + OpenAI 客户端 + 故障转移验证 + 详细日志 |
| ✅ 完成 | ~~安全修复~~ | key 泄露清理 + pre-commit hook + git 历史清理 |
| ✅ 完成 | ~~代码审查 11 个问题~~ | 5 严重 + 4 中等 + 2 低优先级全部修复，含并发安全、性能优化、防御性编程 |
| ✅ 完成 | ~~并发压力测试~~ | 跨请求 20 goroutine + AdaptiveConcurrency 100 并发采样，`-race` 无报告 |
| — | 推送到远程 | 本地领先 origin/master 5 个 commit（待推送） |
| — | MiMo TTFB ~8s | 服务端固定开销（视觉编码 + 预填充），客户端无法优化 |
| P4 | 主动健康检查 | 定期 ping provider 检测恢复，当前仅被动熔断（reset_timeout 后半开试探） |
| P4 | 加权负载均衡 | 当前仅 priority failover，可扩展为 weighted round-robin |
| **P5** | **上下文感知描述** | **下一迭代规划，详见下方第 12 节设计方案** |

---

## 12. P5 设计方案：上下文感知图片描述

> **状态**：规划中，尚未实施。plan 文档（`.trae/documents/plan-tool-result-and-body-limit.md`）中标注为延后的 B 项。

### 问题

当前 `VisionProvider.DescribeImage` 只接收裸图片数据，零对话上下文：

```go
DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error)
```

用户问"这个报错怎么解决？"附了一张截图 → vision provider 只能生成"一个代码编辑器界面"这种通用描述，无法聚焦到报错信息。描述质量受限于"盲描述"。

### 目标

把最近 N 轮对话（user/assistant 交替）的文本提取出来，传给 vision provider，让其生成贴合上下文的图片描述。

### 设计方案

#### 1. 接口扩展（向后兼容）

新增带上下文的方法，保留原方法不变：

```go
type VisionProvider interface {
    DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error)

    // DescribeImageWithContext 带对话上下文描述图片。
    // contextText 是最近 N 轮对话的纯文本摘要，可为空（等价于 DescribeImage）。
    // 默认实现：如果 provider 未实现此方法，回退到 DescribeImage。
    DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error)
}
```

**向后兼容策略**：在 `vision/provider.go` 中定义一个 `BaseProvider` 嵌入结构体，提供 `DescribeImageWithContext` 的默认实现（直接调用 `DescribeImage` 忽略 contextText）。各 provider 可选择 override。

#### 2. 上下文提取（`messages/context.go` 新文件）

```go
// ExtractConversationContext 从请求中提取最近 N 轮对话的纯文本。
// 跳过 image 块，只收集 text 块。按时间顺序拼接为 "user: ...\nassistant: ...\n"。
// maxChars 限制总长度，超出时截断早期对话。
func ExtractConversationContext(req *Request, recentRounds int, maxChars int) string
```

- `recentRounds`：默认 3（最近 3 轮 user/assistant 交替）
- `maxChars`：默认 2000（约 500 tokens，控制 vision 调用成本）
- 跳过当前正在处理的图片所在消息（避免把图片的 base64 数据当文本）
- 输出格式：`[user] 前面的问题文本\n[assistant] 上次回答文本\n[user] 当前问题文本`

#### 3. 缓存策略（关键决策）

**方案：缓存 key 仍只 hash 图片内容，不感知上下文。**

理由：
- 同一张截图在不同对话中复用时（如用户多次追问同一张图），首次生成上下文感知描述后，后续命中缓存直接返回，避免重复 vision 调用
- 缓存命中率不下降
- 代价：同一张图在不同上下文下返回首次的描述，可能不完全贴合新上下文
- 权衡：vision 调用成本高（8-30s + API 费用），缓存命中的价值远大于描述精确度的微小损失

#### 4. handler.go 集成

在 errgroup goroutine 内，调用 `DescribeImageWithContext` 替代 `DescribeImage`：

```go
// 在 goroutine 闭包外预提取上下文（所有图片共享同一份上下文）
contextText := messages.ExtractConversationContext(&req, 3, 2000)

// singleflight 闭包内
res.Desc, res.Err = h.deps.VisionProvider.DescribeImageWithContext(
    dedupCtx, blk.Source.Data, blk.Source.MediaType, imageSize, contextText,
)
```

#### 5. Provider 实现

**MiMo（Anthropic API）**：在 messages 数组中图片消息前插入一个 user text block 携带上下文：

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "对话上下文：\n[user] 这个报错怎么解决？\n[assistant] ..."},
    {"type": "image", "source": {"type": "base64", ...}}
  ]
}
```

**OpenAI 兼容客户端**：在 `messages` 数组中图片消息前插入一个 text 消息：

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "对话上下文：\n[user] ..."},
    {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
  ]
}
```

#### 6. 配置

```yaml
vision:
  context_rounds: 3          # 传入最近 N 轮对话，0 = 禁用上下文感知（向后兼容）
  context_max_chars: 2000    # 上下文文本最大字符数
```

- `context_rounds: 0` 时完全等价于当前行为（不提取上下文，调用 DescribeImage）
- 单 provider 模式和多 provider 池模式都支持

#### 7. 文件结构

| 文件 | 改动 | 职责 |
|------|------|------|
| `vision/provider.go` | 修改 | 新增 `DescribeImageWithContext` 方法 + `BaseProvider` 默认实现 |
| `messages/context.go` | **新增** | `ExtractConversationContext` 函数 |
| `messages/context_test.go` | **新增** | 上下文提取测试（轮数截断 / 字符截断 / 跳过 image / 空对话） |
| `vision/client.go` | 修改 | MiMo 客户端 override `DescribeImageWithContext` |
| `vision/openai_client.go` | 修改 | OpenAI 客户端 override `DescribeImageWithContext` |
| `vision/pool.go` | 修改 | Pool 透传 `DescribeImageWithContext` 到底层 provider |
| `config/loader.go` | 修改 | 新增 `ContextRounds` / `ContextMaxChars` 字段 |
| `proxy/handler.go` | 修改 | 预提取上下文 + 调用 `DescribeImageWithContext` |
| `main.go` | 修改 | 传配置到 HandlerDeps |

#### 8. 风险与注意事项

- **token 成本**：上下文增加 vision 调用的 input tokens，但 2000 字符约 500 tokens，影响可忽略
- **延迟**：vision 调用本身 8-30s，上下文预提取 < 1ms，不增加可感知延迟
- **singleflight**：同一张图的 singleflight key 不变（仍只 hash 图片内容），上下文不参与去重 key
- **向后兼容**：`context_rounds: 0` 完全禁用，行为与当前一致；旧 provider 不实现新方法也能工作（默认实现回退）
- **上下文泄露**：上下文文本会发送给 vision provider，需确认无敏感信息（与图片本身发送给 vision provider 的风险一致）

## 10. 环境事实

- **Go 版本**：go 1.22+（用了 loopvar 修复，但代码里仍显式 `i, blk := i, blk`）
- **Windows PowerShell**：环境变量用 `$env:VAR = "value"` 语法
- **CC Switch 代理模式会截断图片数据**（316→188 bytes），必须用 `ANTHROPIC_BASE_URL` 直连
- **MiMo 端点**：`https://api.xiaomimimo.com/anthropic`（Anthropic 格式），不是 `/v1`（OpenAI 格式）
- **真实 API key** 在 `config.yaml`（gitignore），不在代码里
- **pre-commit hook** 已配置（`git config core.hooksPath .githooks`），扫描 `sk-[A-Za-z0-9]{20,}` 阻止 key 泄露
- **git 历史已清理**：旧 commit 中的 API key 已用 `git filter-repo` 替换为 `REDACTED_USE_ENV_VAR`
- **测试脚本用环境变量**：`smoke-fill-samples.py` 和 `test-failover.py` 通过 `BLIND_LLM_EYES_API_KEY` 环境变量读取 key

## 11. 相关文档

| 文件 | 内容 |
|------|------|
| [CONCURRENCY_DESIGN.md](./CONCURRENCY_DESIGN.md) | errgroup 并发优化 + concurrency_limit 设计文档 |
| [docs/superpowers/specs/2026-08-12-multi-vision-provider-pool-design.md](./docs/superpowers/specs/2026-08-12-multi-vision-provider-pool-design.md) | P3 多 Provider 池设计规格文档 |
| [vision-fallback-architecture.md](./vision-fallback-architecture.md) | 原始架构设计 v1 |
| [vision-fallback-notes.md](./vision-fallback-notes.md) | 决策时间线 + 调研记录 |
| [CONTEXT.md](./CONTEXT.md) | 项目上下文 |
