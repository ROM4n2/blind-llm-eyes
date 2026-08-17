# 交接文档 — blind-llm-eyes 视觉代理

> **最近更新**：2026-08-17（Tier 2 v1.1.0-dev 迭代 M0+M1.A+M1.B+perf 优化完成，已推送远程；M1.C/D/M2/M3 待办）
> **当前版本**：v1.1.0-dev（unreleased）。GA 版本仍为 [v1.0.1](./RELEASE_NOTES-v1.0.1.md)
> **状态**：
> - **v1.0.0 GA**（2026-08-13）：MVP → P1 → P2 → P3 → P5 → 产品化 11 项 TDD 任务全部完成。`go test -race ./...` 全 13 包全绿；`goreleaser build --snapshot` 6 平台二进制全成功；master 已合并，tag `v1.0.0` 本地就位。详见 [RELEASE_NOTES-v1.0.0.md](./RELEASE_NOTES-v1.0.0.md)
> - **Tier 2 v1.1.0-dev**（2026-08-15 ~ 2026-08-16）：M0 接口锁定 + M1.A SQLite 缓存 + M1.B Qwen setup + 两个 perf 优化补丁共 15 个 commit 已推送 `origin/master`。完整变更摘要见 [CHANGELOG.md](./CHANGELOG.md)，任务清单见 [plan](./docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md)。**M1.C/D/M2/M3（Task 13-22）待办**，详见下方第 0.5 节

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

24. **T1 buildinfo 包 + version 子命令** (commit `3d9f9eb`)
    - 新建 `buildinfo/buildinfo.go`：`var Version = "dev"`，由 goreleaser 经 ldflags `-X ...buildinfo.Version=1.0.0` 注入
    - CLI 内 `version`：打印 `blind-llm-eyes <Version> (go <runtime.Version()>)`
    - 验证：本地构建 `go build -ldflags "-X ...buildinfo.Version=1.0.0"` → `blind-llm-eyes 1.0.0 (go go1.26.5)`

25. **T2 抽取 vision provider 构造函数** (commit `ad70a9f`)
    - main.go 中 200 行 provider 构造抽成 `vision.BuildProvider(pc, logger)` / `BuildSingleProvider(vc, logger)` / `BuildPool(cfg, logger)`
    - main.go 瘦身为：dispatch → `runServer` 调用 builder
    - 每种 Type 构造单测：`go test ./vision/` 全通过

26. **T3 CLI 子命令分发骨架 + thin main.go** (commit `77f8ee8`)
    - `cli/cli.go`：`Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int`，手写 switch 分发（零 CLI 依赖）
    - main.go：无参数 / `start` / `-config` / `-*` → 走 `runServer`（向后兼容）；其余 → `cli.Run`
    - 8 个子命令：`version` / `setup` / `doctor` / `connect` / `disconnect` / `status` / `stop` / 隐含 `start`
    - 路由表驱动单测：`go test ./cli/ -run TestRun_` 全通过

27. **T4 admin shutdown 端点 + pidfile + status/stop** (commit `405b4b2`)
    - `admin/admin.go`：`POST /admin/shutdown`，`X-Admin-Token` 鉴权（错 token → 403，对 → 202 + 触发 graceful drain）
    - `cli/pidfile.go`：pidfile 存 `os.UserConfigDir()/blind-llm-eyes/pidfile.json`（Windows = `%AppData%\blind-llm-eyes\`），字段 pid/addr/token/startedAt
    - `status`：读 pidfile → `GET /healthz` → `RUNNING` / `STALE`（不信任 pid 存活，Windows pid 复用）
    - `stop`：读 pidfile → POST `/admin/shutdown` 带 token → 清理 pidfile
    - 4 个文件测试 + admin 端点测试 + status/stop 子命令测试全绿

28. **T5 vision Ping + upstream Ping → doctor 子命令** (commit `e2cbf8b`)
    - `vision/Ping(ctx)`：客户端级发送 `max_tokens=1` text-only 请求，语义 = 可达 + 非 401/403；model 级 400 当 warning 不当失败
    - `cli.PingUpstream(ctx, baseURL, apiKey, model)`：对上游 `/v1/messages` 同语义 ping
    - `doctor`：按顺序 ping upstream + 每个 vision provider，打印表；全部通过 exit=0，任一失败 exit=1
    - `go test ./vision/ ./cli/ -run Ping` / `Doctor` 全通过

29. **T6 `[1m]` 模型名剥离** (commit `7a26701`)
    - 新建 `modelutil/modelutil.go`：`SanitizeModel(m string) string`，仅剥尾部 `[<digits><unit>]`（`[1m]`/`[1M]`/`[128K]` 等），不碰中间括号
    - proxy handler 转发前 `req.Model = modelutil.SanitizeModel(req.Model)`（全量缓冲 + remarshal，改写安全）
    - cc-switch 导入层同样经 `SanitizeModel`（双保险）
    - 纯函数表驱动单测 + E2E 断言上游收到剥后 model，全绿

30. **T7 connect/disconnect settings.json 接线** (commit `5565bd6`)
    - `cli/settings.go`：`map[string]json.RawMessage` 往返，只改 `env.ANTHROPIC_BASE_URL`，非 env 顶层键（effortLevel/hooks/theme/tui 等）逐字节保留
    - `connect`：写前整文件备份到 `~/.claude/.bak-before-connect`（备份已存在不覆盖，保证原始状态永远可恢复）；原子写（temp + rename）
    - `disconnect`：从备份整文件字节还原，然后删备份标记
    - golden 测试：非 env 键逐字节保留、重复 connect 备份不变、disconnect 还原一致，`go test ./cli/ -run TestConnect` 全通过

31. **T8 cc-switch SQLite 供应商导入** (commit `252d06e`)
    - 依赖 `modernc.org/sqlite`（纯 Go、无 CGO，三平台 goreleaser 可交叉编译）
    - 打开 `~/.cc-switch/cc-switch.db`：`mode=ro&immutable=1` 只读；文件锁定（cc-switch GUI 运行中）→ 整 DB 拷贝到 temp 再读；仍失败 → setup 回退手填（best-effort）
    - 查询 `app_type='claude'` provider，解析 `settings_config JSON env` → 列出候选供 setup 挑选 upstream/vision；模型经 `SanitizeModel` 后填 vision.model
    - 内存 SQLite 测试：正常抽取 / 空行容错 / 损坏回退 全绿

32. **T9 交互式 setup 向导编排** (commit `1508234`)
    - 纯 stdin 问答流：① cc-switch 导入？（T8）→ ② 手填 upstream/vision（base_url/api_key/model，带默认值 deepseek/mimo）→ ③ doctor 自检（失败询问"仍要保存吗"）→ ④ 可选立即 connect（T7）→ ⑤ 原子写 `config.yaml` → ⑥ 打印启动指引
    - 可测试性：stdin/stdout/写文件/connect 全部接口注入，`bytes.Buffer` 脚本化 stdin 驱动不落盘
    - 3 个测试场景：纯手填 / doctor 失败仍保存 / cc-switch 导入 全通过

33. **T10 goreleaser + release workflow + Makefile + README** (commit `9f46a2b`)
    - `.goreleaser.yaml`：linux/darwin/windows × amd64/arm64 = 6 目标；CGO_ENABLED=0（全依赖纯 Go）；ldflags 注入版本；archives + checksums
    - `.github/workflows/release.yml`：推送 `v*` tag → check out → setup-go → `goreleaser release --clean`
    - `Makefile`：`test`（race）/ `vet` / `build`（VERSION 变量）/ `snapshot` / `goreleaser-check` / `release` / `clean`
    - README Quick start 重写：下载 / setup / connect / start 四步法
    - 验证：`goreleaser check` 通过；`goreleaser build --snapshot --clean` 6 二进制全成功（darwin amd64 15.9M / linux amd64 15.7M / windows amd64 16.0M 等）

34. **T11 E2E 集成测试** (commit `29c5104`)
    - `test/e2e_test.go`：httptest 假 DeepSeek + 假 MiMo，真实 `vision.Client` + `proxy.NewHandler`
    - `TestE2E_FullPipeline`：`model:"deepseek-chat[1m]"` + 1×1 PNG → 断言上游收到 `deepseek-chat`（T6 生效）、MiMo 恰好被调用 1 次并看到 `mimo-v2.5`、图片被替换为描述、SSE 透传、`X-Blind-Llm-Eyes: rewritten=1 cached=0`、第 2 次请求缓存 hit（vision 调用不增）
    - `TestE2E_AdminShutdown_PidfileCleanup`：真实 `WritePidfile` + `admin.ShutdownHandler`；错 token → 403 + 不关闭；对 token → 202 + `Done()` 关闭 + pidfile 删除
    - `go test -race -count=1 ./test/` 全通过

35. **网络超时健壮性 E2E（T11 增强）** (commit `3f059f8`)
    - `TestE2E_VisionTimeout_FailOpen`：慢 MiMo（2s）+ 客户端 200ms 超时 + `FailOpen=true` → HTTP 200，上游收到占位符 `[Image could not be described by vision model]` 而非图片或延迟描述
    - `TestE2E_VisionTimeout_FailClosed`：同慢服务器 + `FailOpen=false` → HTTP 502，上游永不触达，响应体含 `"vision call failed"`
    - **goroutine 生命周期安全**：handler 用 `select`+`done` channel 模式（非裸 `time.Sleep`），`httptest.Server.Close()` 不阻塞
    - `-race` 全通过，无数据竞争

36. **Release Notes + 合并 master + 打 tag** (commit `08dc4bd` + merge `58c1ee8`)
    - 生成 [RELEASE_NOTES-v1.0.0.md](./RELEASE_NOTES-v1.0.0.md)：7 大新功能 / 改进 / 测试（5 E2E 表格）/ 发布基础设施 / 升级指引 / 12 提交完整列表 / 已知限制 / 验证总结
    - `feat/onboarding-productize` → `master` 合并（`--no-ff` merge commit `58c1ee8`）
    - annotated tag `v1.0.0` 打在 master HEAD（待用户推送远程 + 设置 GITHUB_TOKEN 后触发 release workflow）

### 当前状态
- **MVP → P1 → P2 → P3 → P5 → 产品化 v1.0.0 全部完成**
- **go test -race ./... 全绿（13 包全通过）**，`go build` / `go vet` 零警告
- **服务可运行**：`blind-llm-eyes start` 或 `go run . -config config.yaml`，监听 127.0.0.1:8790
- **多 Provider 池已验证**：故障转移 + 熔断器 + 跳过逻辑端到端通过
- **安全防护已就位**：pre-commit hook 阻止 key 泄露，git 历史已清理
- **并发安全已验证**：跨请求压力测试（20 goroutine + 100 并发采样）`-race` 无报告
- **产品化验证通过**：`doctor` 双端 ping PASS、goreleaser 6 平台 snapshot 构建成功、E2E 5 用例全过、status/stop pidfile 流程有效
- **master 已合并，tag v1.0.0 本地就位**（待 `git push --tags` 推送）

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
| ✅ P5 | ~~上下文感知描述~~ | **已完成**：ContextualVisionProvider 接口 + 上下文提取 + client 注入 + Pool 透传 + 配置化 + Handler 集成 + 日志增强 + maxChars 截断 bug 修复 |
| ✅ | ~~failover 时间预算被共享 deadline 饿死~~ | **2026-08-13 修复（产品化前）**。singleflight fn 改为 `WithCancel(Background)` 无硬截止；Pool 每次尝试独立 `WithTimeout(parent, provider timeout)` 子 ctx，fallback 拿到从 parent 派生的全新子 ctx 不受前者耗时影响。3 个回归测试全通过 |
| ✅ T1 | ~~buildinfo 包 + version 子命令~~ | **已完成**（产品化 T1，commit `3d9f9eb`） |
| ✅ T2 | ~~vision provider 构造抽取~~ | **已完成**（T2，`ad70a9f`） |
| ✅ T3 | ~~CLI 分发骨架 + thin main~~ | **已完成**（T3，`77f8ee8`） |
| ✅ T4 | ~~admin + pidfile + status/stop~~ | **已完成**（T4，`405b4b2`） |
| ✅ T5 | ~~vision/upstream Ping + doctor~~ | **已完成**（T5，`e2cbf8b`） |
| ✅ T6 | ~~`[1m]` 模型名剥离~~ | **已完成**（T6，`7a26701`） |
| ✅ T7 | ~~connect/disconnect 接线~~ | **已完成**（T7，`5565bd6`） |
| ✅ T8 | ~~cc-switch SQLite 导入~~ | **已完成**（T8，`252d06e`） |
| ✅ T9 | ~~setup 交互式向导~~ | **已完成**（T9，`1508234`） |
| ✅ T10 | ~~goreleaser + workflow + Makefile~~ | **已完成**（T10，`9f46a2b`） |
| ✅ T11 | ~~E2E 集成测试（含网络超时场景）~~ | **已完成**（T11，`29c5104` + `3f059f8`） |
| 🔴 发布 | 推送 master + tag + 上传 Release Assets | **阻塞于 GITHUB_TOKEN**。用户需在终端设置 `$env:GITHUB_TOKEN='ghp_...'` 后执行：`git push origin master; git push origin v1.0.0`，release workflow 自动出包；或本地 `goreleaser release --clean` |
| 🔴 BUG | setup/cc-switch 导入可产出自循环上游 | 见第 9 节：cc-switch 里"已手动接到代理"的 provider 被当上游导入 → `upstream.base_url`=代理自身 → 无限自循环 |
| 🔴 BUG | handler 无自转发防御 | 见第 9 节：任何 config 让 `upstream.base_url == 自身监听地址` 都无限循环，需 fail fast + doctor 检测 |
| — | MiMo TTFB ~8s | 服务端固定开销（视觉编码 + 预填充），客户端无法优化 |
| P4 | 主动健康检查 | 定期 ping provider 检测恢复，当前仅被动熔断（reset_timeout 后半开试探） |
| P4 | 加权负载均衡 | 当前仅 priority failover，可扩展为 weighted round-robin |
| P4 | README 中文翻译 | `README.zh-CN.md` 未创建（Glob 验证缺失） |
| P4 | daemon / systemd 模式 | 当前仅前台 serve，后台 daemon 化留给 P4 |

---

## 0.5 Tier 2 (v1.1.0-dev) 迭代总结

> **迭代范围**：2026-08-14（设计）~ 2026-08-16（perf 优化补丁），共 15 个 commit
> （`0758431..de514b1`），对应
> [spec](./docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md) 与
> [plan](./docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md) 的 Task 1-12
> （M0 接口锁定 + M1.A SQLite 缓存 + M1.B Qwen setup）+ 两个
> [perf 优化补丁](./docs/superpowers/plans/2026-08-16-sqlite-inmemory-counter.md)
> （内存计数器 + CAS 防并发 evict 风暴）。
>
> **完整变更摘要**见 [CHANGELOG.md](./CHANGELOG.md)。以下仅列接手者必须了解的要点。

### 已完成里程碑

| 里程碑 | Task | Commit | 要点 |
|---|---|---|---|
| **M0** 接口锁定 | 1-3 | `e239a96` `245487e` `eb1197d` | `cache.Cache` 接口解耦 handler；`qwen` provider 类型 stub；`cache` CLI 子命令 stub |
| **M1.A** SQLite 缓存 | 4-11 | `5d193c7` `8dc4afd` `4a9e9ff` `b993018` `cd32794` `1991fa1` `1d69b5e` | SQLite 冷层（WAL + UPSERT + LRU/TTL 双淘汰 + 损坏自愈）；TwoTier 复合层；CacheCfg 扩展；main.go 装配 + LRU-only 降级 |
| **M1.B** Qwen setup | 12 | `de514b1` | setup 向导 Qwen 选项（1=GLM / 2=Qwen / 3=MiMo 手动 / 4=OpenAI 手动）；GLM/Qwen 预设统一输出 `vision_providers + type`；修正 GLM 走 MiMo Client 打 `/v1/messages` 404 的 bug |
| **perf 优化** | — | `d707a2e` `656bab4` | `atomic.Int64` 内存计数器消除每次 Put 的全表 COUNT（10k 库 ~1ms→0）；`evicting atomic.Bool` CAS 防护并发 evict 风暴（50-goroutine + barrier + `-race -count=10` 0 失败） |

### 关键设计决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 缓存接口 | `cache.Cache` 接口（`Get` / `Put`） | 解耦 handler 与具体实现，支持 LRU/TwoTier 无缝切换，编译期断言保证实现一致性 |
| 持久化驱动 | `modernc.org/sqlite`（纯 Go） | 保持 `CGO_ENABLED=0`，goreleaser 三平台交叉编译不破坏 |
| SQLite 并发 | WAL 模式 + `synchronous=NORMAL` | 读写并发安全；单写者串行化保证关键不变式 |
| 损坏恢复 | `PRAGMA integrity_check` + `applyPragmas` 双触发 → 删除 db/-wal/-shm 重建空库 | 冷启动丢失描述但不阻塞服务（best-effort） |
| 装配降级 | `type=twotier` SQLite 打开失败 → 降级 LRU-only + WARN 日志 | 保证服务可用性，冷层失败不阻塞主流程 |
| 缓存计数 | `atomic.Int64` 内存计数器 + `INSERT OR IGNORE` 区分新增/更新 | 消除 O(N) 全表 COUNT；SQLite 单写者 + WAL 原子性保证 count 最终一致 |
| evict 防风暴 | `atomic.Bool` CAS 闸门 | 突发并发场景只允许一个 goroutine 执行 DELETE，用短暂超量换避免风暴 |

### 配置扩展（`config/loader.go`）

`CacheCfg` 新增字段，默认值保留 v1.0.1 行为（`Type=lru`）：

```yaml
cache:
  type: lru              # "lru"（默认，向后兼容）| "twotier"（opt-in 持久化）
  max_entries: 500       # LRU 热层容量
  db_path: ""            # SQLite 路径，空 = os.UserConfigDir()/blind-llm-eyes/cache.db
  sqlite_max_entries: 10000  # SQLite 容量阈值，<=0 回退 10000
  sqlite_ttl: ""         # SQLite TTL（如 "168h"），格式错误会被拒绝
```

### Qwen-VL 预设配置（vision/provider.go）

`qwen` provider 类型对接 DashScope（阿里云百炼）OpenAI 兼容接口，自动填充：

```yaml
vision_providers:
  - name: qwen
    type: qwen            # 自动填 base_url + model
    api_key: sk-...      # 用户只需配置 api_key
```

自动填充值：`base_url=https://dashscope.aliyuncs.com/compatible-mode/v1`，`model=qwen-vl-plus`。复用 `*OpenAIClient`（与 `glm_free` 同路径）。

### 测试覆盖

| 变更文件 | 测试文件 | 覆盖范围 |
|---|---|---|
| `cache/sqlite.go` | `cache/sqlite_test.go` | Open / idempotent reopen / corruption recovery / Get-Put / eviction by count / eviction by TTL / 内存计数器 Put/evict/rebuild / **CAS 防风暴 50-goroutine** |
| `cache/twotier.go` | `cache/twotier_test.go` | Get 回填 / Put 双写 / 50-goroutine 防惊群 |
| `config/loader.go` | `config/loader_test.go` | 默认值 / twotier 解析 / bad-type 拒绝 |
| `vision/provider.go` | `vision/provider_test.go` | Qwen auto-fill / 用户 override 优先 / 空 api_key 报错 |
| `cli/cache.go` + `cli/cli.go` | `cli/cli_test.go` | `TestRun_Routing` 覆盖 cache no-args / unknown / stats stub |
| `cli/setup.go` | `cli/setup_test.go` | 手动 MiMo / doctor 失败保存/取消 / connect / 默认值 / GLM 预设 vision_providers / Qwen 预设 / GLM 统一输出 |

- `go vet ./...` 通过
- `go build ./...` 通过
- `go test -race -count=1 ./...` 全部 PASS（13 个包，race-clean）

### 环境配置变更（本会话）

- **origin URL 改为 SSH**：`git@github.com:ROM4n2/blind-llm-eyes.git`
  （原 HTTPS 在本网络环境 21s 超时，SSH 端口 22 畅通）
- **永久环境变量**（User 级）：`GIT_SSH=C:\WINDOWS\System32\OpenSSH\ssh.exe` + `HOME=C:\Users\Haoyu`
  （让 git 用 Windows OpenSSH 而非 msys2 ssh，找到 ssh-agent 的 ED25519 key）

### Commit List

| Commit | Type | Summary |
|---|---|---|
| `0758431` | docs | add tier2 sqlite cache and qwen-vl preset design |
| `941e426` | docs | add tier2 implementation plan |
| `e239a96` | refactor(cache) | introduce Cache interface to decouple handler |
| `245487e` | feat(vision) | add qwen provider type for DashScope Qwen-VL |
| `eb1197d` | feat(cli) | add cache subcommand stub and usage |
| `5d193c7` | feat(cache) | add sqlite open with schema and wal pragmas |
| `8dc4afd` | feat(cache) | add sqlite get/put with upsert and last_accessed |
| `4a9e9ff` | feat(cache) | add sqlite lru and ttl eviction |
| `b993018` | feat(cache) | add sqlite integrity check and corruption recovery |
| `cd32794` | feat(cache) | add two-tier lru+sqlite composite cache |
| `1991fa1` | feat(config) | extend cachecfg with type dbpath and ttl |
| `1d69b5e` | feat(main) | wire two-tier cache with lru-only fallback |
| `de514b1` | feat(cli) | add qwen preset and unify preset output to vision_providers |
| `d707a2e` | perf(cache) | avoid per-put count(*) via in-memory counter |
| `656bab4` | fix(cache) | add cas guard to prevent concurrent evict storm |
| `20732ab` | docs | add changelog for tier2 m0+m1.a iteration |

### 下一步建议（按优先级）

| 优先级 | 任务 | 说明 |
|---|---|---|
| 🔴 M1.C | `cache stats/list/clear/path` 子命令实现 | Task 13-17。当前 `cli/cache.go` 仍是 stub（返回 "not implemented yet"），需要实现与 SQLite/LRU 缓存交互的统计、列出、清除、显示路径功能。Task 17 完成后删 `runCache` 的 stub 分支 |
| 🔴 M1.D | 跨重启缓存存活 e2e 测试 + 用户文档 | Task 18-20。`test/e2e_test.go` 当前 3 处仍用 `cache.NewLRU(10)`，未覆盖 TwoTier 路径；`main.go` 装配逻辑无单元测试，待 e2e 覆盖；`config.example.yaml` 需文档化 cache 块；README / RELEASE_NOTES 需提及新功能 |
| 🔴 M2 | RC1 | Task 21。tag RC1 + 本地构建验证（`goreleaser build --snapshot` 6 平台） |
| 🔴 M3 | GA | Task 22。tag v1.1.0 + 发布（推送 `v1.1.0` tag 触发 release workflow） |
| ⚠️ v1.0.0 BUG | setup/cc-switch 导入可产出自循环上游 | 见下方第 9 节：cc-switch 里"已手动接到代理"的 provider 被当上游导入 → `upstream.base_url`=代理自身 → 无限自循环。**未修复**，M1.C/D 阶段建议顺手解决 |
| ⚠️ v1.0.0 BUG | handler 无自转发防御 | 见下方第 9 节：任何 config 让 `upstream.base_url == 自身监听地址` 都无限循环，需 fail fast + doctor 检测。**未修复**，M1.C/D 阶段建议顺手解决 |

---

## 1. 项目一句话

本地轻量 Go 代理，架在 **Claude Code ↔ 上游 LLM** 之间：Claude Code 发请求含图片块 → 本地工具用 MiMo 视觉模型把图转成文字描述 → 替换图片块 → 转发给纯文本上游 DeepSeek → 流式响应原样透传。让用户**留在 DeepSeek、不切供应商、粘贴截图直接可用**地看图。

## 2. 当前架构

```
┌───────────────────────────────────────────────────────────────────────┐
│  用户层（CLI 子命令）                                                │
│  blind-llm-eyes setup │ doctor │ connect │ disconnect │ status │     │
│                stop │ version │ start (隐含)                         │
│                ▲                                                     │
│                │ cli.Run(args) 分发                                 │
└────────────────┼──────────────────────────────────────────────────────┘
                 │
                 ▼
Claude Code ──POST /v1/messages──▶ blind-llm-eyes (127.0.0.1:8790)
                                    │
                                    ├─ GET  /healthz       ← status 判活
                                    ├─ GET  /metrics
                                    ├─ POST /admin/shutdown ← stop (X-Admin-Token 鉴权)
                                    │     (token 仅存 pidfile，不持久化)
                                    │
                                    ├─ 解析请求，modelutil.SanitizeModel() 剥 [1m] 后缀
                                    ├─ 提取 image blocks
                                    ├─ 查 LRU 缓存 (SHA-256 hash)
                                    │   ├─ 命中 → 直接用缓存的描述
                                    │   └─ miss → singleflight.Do(hash)
                                    │              ├─ 首个调用者 → Pool.DescribeImageWithContext()
                                    │              │         │
                                    │              │         └─ messages.ExtractConversationContext()
                                    │              │             (最近 N 轮对话，maxChars 截断)
                                    │              │         └─ 写缓存
                                    │              └─ 等待者 → 共享结果
                                    ├─ errgroup 并行处理多图 (concurrency_limit=6, adaptive [1,12])
                                    ├─ 替换 image block → text block (或 fail-open 占位符)
                                    └─ 转发给 DeepSeek (anthropic 端点) ──▶ 流式响应透传
                                                 ▲
                                                 │ Ping(doctor) 轻量可达性+鉴权自检

Pool 内部（vision/pool.go）:
  ┌─ Provider[0] (priority=1, e.g. MiMo)  ← CircuitBreaker: Closed/Open/Half-open
  ├─ Provider[1] (priority=2, e.g. GPT-4o) ← CircuitBreaker: Closed/Open/Half-open
  └─ ...按优先级遍历，跳过熔断器开启的，失败时自动故障转移
       每个 provider 有独立 WithTimeout 子 ctx（避免共享 deadline 饿死 fallback）
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
├── main.go                     # 入口：薄分发（无参数/start → runServer；其余 → cli.Run）
├── config.yaml                 # 运行配置（gitignore，见 config.example.yaml）
├── Makefile                    # test / vet / build / snapshot / release / clean
├── .goreleaser.yaml            # goreleaser 6 平台交叉编译配置
├── .githooks/pre-commit        # API key 泄露检测（sk-[A-Za-z0-9]{20,}）
├── RELEASE_NOTES-v1.0.0.md     # v1.0.0 发布说明（7 大功能 + 5 E2E 用例 + 发布流程）
├── .github/workflows/
│   └── release.yml             # tag push → goreleaser release
├── buildinfo/                  # T1：版本号包
│   ├── buildinfo.go            # var Version = "dev"（ldflags 注入）
│   └── buildinfo_test.go
├── modelutil/                  # T6：模型名工具（纯函数）
│   ├── modelutil.go            # SanitizeModel() 剥尾部 [1m]/[1M]/[128K]
│   └── modelutil_test.go       # 表驱动纯函数测试 + proxy 集成 E2E
├── cli/                        # T3-T9：全部子命令实现
│   ├── cli.go                  # Run(args, stdin, stdout, stderr) int 分发入口
│   ├── cli_test.go             # 路由表驱动测试
│   ├── pidfile.go              # WritePidfile / ReadPidfile / RemovePidfile（原子写）
│   ├── pidfile_test.go         # pidfile 往返、stale 清理
│   ├── status.go               # status 子命令（pidfile + GET /healthz）
│   ├── status_test.go
│   ├── stop.go                 # stop 子命令（pidfile 取 token → POST /admin/shutdown）
│   ├── stop_test.go
│   ├── ping_upstream.go        # PingUpstream()：上游 `/v1/messages` 可达性自检
│   ├── ping_upstream_test.go
│   ├── doctor.go               # doctor 子命令（上游 ping + 每个 vision provider ping）
│   ├── doctor_test.go
│   ├── settings.go             # settings.json 读写（RawMessage 往返保留非 env 键）
│   ├── connect.go              # connect / disconnect 子命令（备份 + 原子写 + 还原）
│   ├── connect_test.go         # golden 测试
│   ├── ccswitch.go             # cc-switch SQLite 导入（modernc.org/sqlite，只读 + 锁回退）
│   ├── ccswitch_test.go        # 内存 SQLite 测试
│   ├── setup.go                # setup 向导（stdin/stdout + 写 config + connect 全部注入）
│   ├── setup_test.go           # 脚本化 stdin 测试（手填 / doctor失败仍存 / cc-switch导入）
│   └── stubs.go                # （已清空占位，所有命令均实现）
├── admin/                      # T4：管理端点
│   ├── admin.go                # ShutdownHandler（X-Admin-Token 鉴权 → Done() 关闭）
│   └── admin_test.go           # 403 拒 token / 202 触发 / 重复调用
├── test/                       # T11 + 网络超时增强
│   └── e2e_test.go             # 5 个 E2E 用例：FullPipeline / AdminShutdown / ShutdownRejectToken / VisionTimeout_FailOpen / VisionTimeout_FailClosed
├── config/
│   └── loader.go               # YAML 配置加载 + ProviderCfg + CircuitBreakerCfg
├── messages/                   # Anthropic Messages API 请求解析/改写/上下文提取
│   ├── content.go              # ContentBlock 结构
│   ├── parse.go                # 请求解析
│   ├── rewrite.go              # 图片块替换为文字 + NormalizeSystemMessages
│   ├── context.go              # P5：ExtractConversationContext()（最近 N 轮对话）
│   ├── context_test.go         # P5：9 个上下文提取测试
│   ├── normalize_test.go       # system message 规范化测试
│   └── validate.go             # 请求校验
├── vision/                     # Vision Provider 层
│   ├── provider.go             # T2：VisionProvider / ContextualVisionProvider 接口 + BuildProvider() / BuildSingleProvider() / BuildPool()
│   ├── provider_test.go        # T2：每种 Type 构造正确 / 空配置报错 / 重复构造不 panic
│   ├── ping.go                 # T5：Client / SingleProvider / Ping(ctx) error（max_tokens=1 轻量）
│   ├── ping_test.go            # T5：Ping 请求体/响应/错误分法测试
│   ├── client.go               # MiMo 客户端（Anthropic API + WebP + httptrace + DescribeImageWithContext）
│   ├── openai_client.go        # 通用 OpenAI 兼容客户端（/v1/chat/completions + DescribeImageWithContext）
│   ├── pool.go                 # 多 Provider 池（优先级 failover + 独立子 ctx 防饿死 fallback）
│   ├── circuit_breaker.go      # 三态熔断器 + Stats() 状态快照
│   ├── pool_test.go            # 池测试（优先级/故障转移/熔断/并发 + failover 不饿死 + per-provider timeout）
│   └── circuit_breaker_test.go # 熔断器测试（状态转换/半开/并发）
├── proxy/
│   ├── handler.go              # 核心代理逻辑（SanitizeModel → errgroup + singleflight → AIMD → 安全过滤）
│   ├── adaptive.go             # AIMD 自适应限流控制器
│   ├── passthrough.go          # 流式响应透传
│   ├── handler_test.go         # 基础图片替换 + 缓存 round-trip
│   ├── handler_concurrency_test.go  # errgroup 并行 + concurrency_limit 配置化 + 自适应跨请求
│   ├── handler_singleflight_test.go # singleflight stampede / 跨请求 / ctx 隔离
│   ├── handler_modelutil_test.go    # T6：SanitizeModel proxy 集成 E2E
│   ├── handler_integration_fix_test.go  # 5 个严重问题修复集成测试
│   ├── handler_medium_fix_test.go       # 4 个中等问题修复 + 端到端嵌套测试
│   └── handler_race_stress_test.go      # 跨请求并发压力测试（-race）
├── cache/
│   ├── lru.go                  # LRU 缓存
│   └── hash.go                 # SHA-256 内容哈希
├── logging/
│   └── logging.go              # 异步 JSON 日志 + request_id 传播
└── metrics/
    └── metrics.go              # Prometheus 指标（含 per-provider 指标 + adaptive_limit 等）
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

### 产品化标准流程（v1.0.0+）

```powershell
# 0. 安装：从 Releases 下载二进制，或从源码构建
#    下载：https://github.com/ROM4n2/blind-llm-eyes/releases
#    源码：go build -ldflags "-X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version=1.0.0" -o blind-llm-eyes.exe .

# 1. 交互式配置（cc-switch 导入 / 手填 / doctor 自检 / 可选 connect / 写 config.yaml）
.\blind-llm-eyes.exe setup

# 2. 接线 Claude Code（改写 ~/.claude/settings.json env.ANTHROPIC_BASE_URL，备份+原子写）
.\blind-llm-eyes.exe connect
#    需要重启 Claude Code 让它重新读 settings.json
#    connect 期间勿用 cc-switch 切供应商（会覆盖 base_url）
#    还原： .\blind-llm-eyes.exe disconnect   （从 .bak-before-connect 字节还原）

# 3. 启动（无参数或 start 均为前台 serve，后台 daemon 化留给 P4）
.\blind-llm-eyes.exe                  # 向后兼容
.\blind-llm-eyes.exe start            # 显式
.\blind-llm-eyes.exe -config C:\path\to\config.yaml   # 指定配置路径

# 4. 生命周期管理（在另一个终端执行）
.\blind-llm-eyes.exe status           # RUNNING / STALE / NOT RUNNING（pidfile + /healthz 双重判活）
.\blind-llm-eyes.exe doctor           # 连通性自检（上游 ping + 每个 vision provider ping），非零=有问题
.\blind-llm-eyes.exe stop             # POST /admin/shutdown token-authed → graceful drain → 删 pidfile
.\blind-llm-eyes.exe version          # blind-llm-eyes 1.0.0 (go go1.26.5)

# 端点
# POST /v1/messages     — 代理接口
# GET  /healthz         — 健康检查（liveness）
# GET  /metrics         — Prometheus 指标
# POST /admin/shutdown  — 优雅关闭（需 X-Admin-Token，只供 stop 子命令使用）
```

### 验证（5 层递进法）

```powershell
# L1: 基础 — 版本号注入正确
.\blind-llm-eyes.exe version          # 期望 blind-llm-eyes 1.0.0 (go goX.Y.Z)

# L2: 连通性 — doctor 双端可达
.\blind-llm-eyes.exe doctor           # 期望 upstream=PASS vision=PASS，exit=0

# L3: 进程存活 — start + status/healthz
# terminal A: .\blind-llm-eyes.exe start
# terminal B:
.\blind-llm-eyes.exe status           # 期望 RUNNING pid=<pid> addr=127.0.0.1:8790
curl http://127.0.0.1:8790/healthz   # 期望 ok

# L4: 端到端链路 — 发带图请求（消耗少量 API 配额）
curl -N http://127.0.0.1:8790/v1/messages `
  -H "Authorization: Bearer sk-upstream-key" `
  -H "Content-Type: application/json" `
  -d '{\"model\":\"deepseek-chat\",\"max_tokens\":500,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"What color is this?\"},{\"type\":\"image\",\"source\":{\"type\":\"base64\",\"media_type\":\"image/png\",\"data\":\"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg==\"}}]}]}'
# 期望: HTTP 200 + X-Blind-Llm-Eyes: rewritten=1 cached=0 + 响应中含图片描述文字

# L5: 优雅关闭
.\blind-llm-eyes.exe stop             # 成功后 status → NOT RUNNING
```

### CC Switch 注意事项

把 DeepSeek 供应商的 `ANTHROPIC_BASE_URL` 指向 `http://127.0.0.1:8790`（**不要用 CC Switch 代理模式**，会截断图片 body 导致 hash 失败/vision 报错；setup/connect 均显式用直连）。

### 从 v0（dev 时期）升级

无需改 config.yaml 格式（产品化改动零破坏）。推荐做法：
1. 解压 v1.0.0 二进制覆盖旧 `blind-llm-eyes.exe`
2. 跑一次 `setup` 复用现有值 + 触发 doctor 双端验证
3. 首次运行 `connect` 自动接管 Claude Code settings.json（之前手动配的 base_url 会被备份）

## 7. 测试

```powershell
# 全量测试（含 race detector，CI 门禁）
go test -race -count=1 ./...

# 仅跑产品化 CLI + E2E
go test -race -v ./cli/ ./admin/ ./buildinfo/ ./modelutil/ ./test/

# 仅跑 E2E（全链路 + 网络超时场景）
go test -race -v ./test/
```

测试覆盖（v1.0.0 产品化后全量）：

**基础层**
- `buildinfo/buildinfo_test.go` — 版本号默认值 / 格式校验
- `modelutil/modelutil_test.go` — `SanitizeModel` 表驱动纯函数 + proxy handler 集成 E2E
- `config/*` — YAML 加载/默认值/env 覆盖（无新增测试，向后兼容）

**CLI 子命令层（全部 TDD，20+ 个新测试）**
- `cli/cli_test.go` — 子命令路由表（version/setup/doctor/connect/disconnect/status/stop/unknown），未知命令非零退出
- `cli/pidfile_test.go` — pidfile 往返 JSON、stale 进程 pid 检查、清理
- `cli/status_test.go` — RUNNING / STALE / NOT RUNNING 三态（pidfile + mock healthz 交叉）
- `cli/stop_test.go` — 正确 token 触发 shutdown + 清 pidfile；stale 报错提示清理
- `cli/ping_upstream_test.go` — httptest 模拟：200 OK / 401 鉴权失败 / 400 model 级 warning 不当失败 / 网络超时
- `cli/doctor_test.go` — 全部 PASS 零退出、任一 FAIL 非零、坏配置路径报错
- `cli/connect_test.go`（golden）：settings.json 非 env 键逐字节保留、重复 connect 不覆盖备份、disconnect 字节还原与备份一致、空 settings 生成新 + 备份标记
- `cli/ccswitch_test.go` — 内存 SQLite：正常 providers 抽取、空行跳过容忍、损坏/锁 DB 失败回退手填
- `cli/setup_test.go`（脚本化 stdin）：纯手填 full flow → config.yaml 字段全对 / doctor 失败默认不保存（用户确认才存）/ cc-switch 导入 pick 候选

**Vision + Provider**
- `vision/provider_test.go`（T2 新增）— mimo/openai 类型构造正确、空 model/base_url 报错、重复构造不 panic（metrics 注入式安全）
- `vision/ping_test.go`（T5 新增）— 无图 body + max_tokens=1、2xx/401/400/网络错 三分法、Pool 多 provider 逐个 ping
- `vision/client_test.go` — MiMo Anthropic API 调用（含 thinking.disabled）
- `vision/openai_client_test.go` — OpenAI 兼容客户端
- `vision/pool_test.go` — 优先级排序 / 故障转移 / 熔断跳过 / 全部失败 / 并发安全 + **failover 不饿死 fallback（独立子 ctx timeout）** + per-provider large timeout 选择
- `vision/circuit_breaker_test.go` — 状态转换 / 半开单试探 / reset_timeout / 并发安全

**Admin 端点（T4）**
- `admin/admin_test.go` — 缺 token → 403、错 token → 403、对 token → 202 + Done() 关闭、重复调用 Done() 安全

**Proxy Handler**
- `proxy/handler_test.go` — 基础图片替换 + 缓存 round-trip
- `proxy/handler_concurrency_test.go` — errgroup 并行（5 图 × 2s mock 验证 4+1 批次）+ concurrency_limit 配置化 + 自适应跨请求集成
- `proxy/handler_singleflight_test.go` — singleflight stampede / 跨请求 / ctx 隔离
- `proxy/handler_modelutil_test.go`（T6 新增）— `deepseek-chat[1m]` → 上游收到 `deepseek-chat`，其余字段逐字节不变
- `proxy/handler_integration_fix_test.go` — 5 个严重问题修复集成（race / timeout / marshal / cache pollution / header leak）
- `proxy/handler_medium_fix_test.go` — 4 个中等问题修复 + 嵌套 tool_result 端到端
- `proxy/handler_race_stress_test.go` — 跨请求并发压力（20 goroutine 混合图片 + AdaptiveConcurrency 100 并发采样）
- `proxy/adaptive_test.go` — AIMD 控制器 increase / decrease / error-trigger / hysteresis / disabled-is-static

**Messages（含 P5 上下文）**
- `messages/context_test.go`（P5 新增 9 个）— nil/empty、rounds=0 禁用、单轮、多轮截断、maxChars 截断、双消息超限、跳过 image、嵌套 tool_result、最后 user 含 image 跳过
- `messages/normalize_test.go` — system message 规范化（5 场景）
- `messages/*_test.go` — 请求解析/改写/校验 + 嵌套 tool_result round-trip

**E2E 集成（T11 + 超时增强）**
- `test/e2e_test.go` — 5 个端到端用例：
  - `TestE2E_FullPipeline`：`model:"deepseek-chat[1m]"` 剥后 → 真实 vision.Client + 假 MiMo → handler → 假 DeepSeek → SSE 透传，断言 rewritten/cached/描述/上游 model；第 2 次请求缓存 hit
  - `TestE2E_AdminShutdown_PidfileCleanup`：真实 `WritePidfile` + `admin.ShutdownHandler`，错 token 403/好 token 202+Done+清 pidfile
  - `TestE2E_AdminShutdown_RejectsMissingToken` — 无 token POST 被拒（403）+ handler 仍 armed
  - `TestE2E_VisionTimeout_FailOpen`（新增）— 慢 vision + 短超时 + FailOpen=true → 200 + 占位符描述，上游收到占位符
  - `TestE2E_VisionTimeout_FailClosed`（新增）— 慢 vision + FailOpen=false → 502，上游永不触达，响应体含 vision call failed

**脚本/冒烟（手工运行）**
- `test-adaptive-prod.py` / `test-adaptive.ps1` — 自适应限流端到端（mock MiMo + 三阶段 AIMD）
- `smoke-fill-samples.py` — 生产冒烟（9 请求 × 2 唯一图 = 18 样本填 AIMD 窗口）
- `SMOKE_TEST_REPORT.md` — 生产冒烟报告（20 样本延迟 + AIMD 决策）
- `test-failover.py` — P3 故障转移端到端（5 图验证熔断器开启→跳过）

## 8. 最近 commit

```
58c1ee8 merge: productize blind-llm-eyes for v1.0.0 release
08dc4bd docs: add v1.0.0 release notes
3f059f8 test(e2e): add network timeout scenarios for fail-open and fail-closed paths
29c5104 test: add e2e integration test for full pipeline and admin shutdown
9f46a2b chore: add goreleaser, release workflow, and makefile
1508234 feat(cli): add interactive setup wizard with cc-switch import and doctor
252d06e feat(cli): add cc-switch SQLite provider import
5565bd6 feat(cli): add connect/disconnect for Claude Code settings.json wiring
7a26701 feat(proxy): strip [1m] model suffix before forwarding to upstream
e2cbf8b feat(cli): add doctor subcommand with vision and upstream ping
405b4b2 feat(admin): add shutdown endpoint, pidfile, and status/stop subcommands
77f8ee8 feat(cli): add subcommand dispatch skeleton and thin main.go
ad70a9f refactor(vision): extract provider builders from main.go
3d9f9eb feat(cli): add buildinfo package and version subcommand
```

> 注：git filter-repo 清理 key 后，早期（P1 及之前）commit hash 与 HANDOFF 中引用的旧 hash 不一致，以 `git log` 实际输出为准。

## 9. 已知限制 & 后续方向

| 优先级 | 方向 | 说明 |
|--------|------|------|
| ✅ 完成 | ~~P1 配置化~~ | concurrency_limit + description_cap 从 config.yaml 读取 |
| ✅ 完成 | ~~P2 自适应限流~~ | AIMD 控制器 + 单测 + 集成测试 + 端到端验证 + 冒烟测试 + 配置调优 |
| ✅ 完成 | ~~P3 多 Provider 池~~ | Provider 池 + 熔断器 + OpenAI 客户端 + 故障转移验证 + 详细日志 |
| ✅ 完成 | ~~安全修复~~ | key 泄露清理 + pre-commit hook + git 历史清理 |
| ✅ 完成 | ~~代码审查 11 个问题~~ | 5 严重 + 4 中等 + 2 低优先级全部修复 |
| ✅ 完成 | ~~并发压力测试~~ | 跨请求 20 goroutine + AdaptiveConcurrency 100 并发采样，`-race` 无报告 |
| ✅ 完成 | ~~P5 上下文感知描述~~ | ContextualVisionProvider 接口 + 上下文提取 + client/Pool 透传 + 配置化 + Handler 集成 + maxChars bug 修复 |
| ✅ 完成 | ~~failover 时间预算被共享 deadline 饿死~~ | **2026-08-13 修复**。singleflight fn 改为 `WithCancel(Background)` 无硬截止；Pool 每次尝试独立 `WithTimeout(parent, provider timeout)` 子 ctx，fallback 拿全新子 ctx 不受前者耗时影响。3 个回归测试全通过 |
| ✅ 完成 | ~~T1-T11 产品化 11 项 TDD 任务~~ | buildinfo / 构造抽取 / CLI 骨架 / admin+pidfile / Ping+doctor / `[1m]` 剥离 / connect / cc-switch / setup / goreleaser+workflow / E2E+超时场景。13 commit，`go test -race ./...` 13 包全绿 |
| 🔴 发布 | 推送 master + tag + Release Assets | **阻塞于 GITHUB_TOKEN**。设置 `$env:GITHUB_TOKEN='ghp_...'` 后 `git push origin master; git push origin v1.0.0`，release workflow 自动出包；或本地 `goreleaser release --clean` |
| ⚠️ Trae 沙箱限制 | pidfile tmp 文件被沙箱拒（仅 IDE 终端） | Trae 沙箱不允许在 `%AppData%` 下创建 `*.tmp`（CreateTemp 报错）。**在独立 PowerShell 终端中运行 `status`/`stop` 正常**；前台 start 不受影响 |
| 🔴 BUG | setup/cc-switch 导入可产出自循环上游 | **2026-08-13 实测发现（v1.0.0）**。cc-switch 数据库里**合法存在**"已手动接到代理"的 provider（`base_url=http://127.0.0.1:8790`，名字还叫 DeepSeek）。`setup` 导入上游时选中它 → config.yaml `upstream.base_url` = 代理自身地址 → 代理把所有请求转发给自己 → **无限自循环**（日志无限刷 `parallel_images_start` / `forwarding request to upstream`，url=127.0.0.1:8790，remote_addr 也是 127.0.0.1:8790）。实测：发布版解压目录跑 setup 即复现。**修复方向**：`cli/ccswitch.go` + `cli/setup.go` 导入上游 provider 时，若其 `base_url` 等于代理自身 listen 地址 → 拒绝/警告（cc-switch 导入还应主动过滤掉这类 provider） |
| 🔴 BUG | handler 无自转发防御 | **2026-08-13 实测发现（v1.0.0）**。即使绕过 cc-switch 导入，任何 config 手误让 `upstream.base_url == 自身监听地址` 都无限循环（proxy/handler.go 转发前无此检查）。**修复方向**：handler 构造/转发前检测 `UpstreamBaseURL` 的 host:port 等于自身 `listen` → fail fast 报清晰错误（"upstream 指向代理自身"），而非默默循环；`doctor` 也应检测并报告此情况（上游连通性测试连到自己可能通过或超时，不可靠） |
| — | MiMo TTFB ~8s | 服务端固定开销（视觉编码 + 预填充），客户端无法优化 |
| P4 | 主动健康检查 | 定期 ping provider 检测恢复，当前仅被动熔断（reset_timeout 后半开试探） |
| P4 | 加权负载均衡 | 当前仅 priority failover，可扩展为 weighted round-robin |
| P4 | README 中文翻译 | `README.zh-CN.md` 未创建 |
| P4 | daemon / systemd 模式 | 当前仅前台 serve，后台 daemon 化留给 P4 |

### v1.1.0-dev（Tier 2）后续方向

> **完整任务详情**见 [plan](./docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md) Task 13-22。
> **变更摘要**见 [CHANGELOG.md](./CHANGELOG.md)。

| 优先级 | Task | 说明 | 状态 |
|---|---|---|---|
| ✅ 完成 | ~~M0 接口锁定（Task 1-3）~~ | `cache.Cache` 接口 + `qwen` provider stub + `cache` CLI stub | `e239a96` `245487e` `eb1197d` |
| ✅ 完成 | ~~M1.A SQLite 缓存（Task 4-11）~~ | SQLite 冷层 + TwoTier + CacheCfg 扩展 + main.go 装配降级 | `5d193c7`…`1d69b5e` |
| ✅ 完成 | ~~M1.B Qwen setup（Task 12）~~ | setup 向导 Qwen 选项 + 预设统一输出 `vision_providers` + GLM 404 修正 | `de514b1` |
| ✅ 完成 | ~~perf：内存计数器~~ | `atomic.Int64` 消除每次 Put 的全表 COUNT | `d707a2e` |
| ✅ 完成 | ~~perf：CAS 防风暴~~ | `atomic.Bool` 防护并发 evict 风暴，50-goroutine `-race -count=10` 0 失败 | `656bab4` |
| 🔴 **M1.C** | **Task 13-17：cache CLI 子命令实现** | 当前 `cli/cache.go` 仅 stub，返回 "not implemented yet"。需实现 `path`（最简）/ `stats` / `list` / `clear`，再删 `runCache` 的 stub 分支 | 待办 |
| 🔴 **M1.D** | **Task 18-20：E2E + 用户文档** | `test/e2e_test.go` 3 处仍用 `cache.NewLRU(10)`，未覆盖 TwoTier 路径；`main.go` 装配逻辑无单测（Go 惯例由 e2e 覆盖）；`config.example.yaml` 需文档化 cache 块；README / RELEASE_NOTES 提及新功能 | 待办 |
| 🔴 **M2** | **Task 21：RC1** | tag `v1.1.0-rc1` + `goreleaser build --snapshot` 6 平台构建验证 | 待办 |
| 🔴 **M3** | **Task 22：GA** | tag `v1.1.0` + 推送触发 release workflow | 待办 |
| ⚠️ 顺手 | v1.0.0 自循环 BUG | 见上方表格"setup/cc-switch 导入可产出自循环上游" + "handler 无自转发防御"。M1.C/D 阶段建议顺手解决 | 待办 |

---

## 12. P5 上下文感知图片描述（已实现）

> **状态**：✅ 已完成。4 个 commit（`5329945` 接口层 → `d1a2db8` client 层 → `885852b` 配置+集成 → `aee0b15` bug 修复），go build + go vet + go test -race ./... 全绿。

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

#### 9. 实现总结（2026-08-13）

**实际实现与设计方案的差异**：
- 接口设计：未使用 `BaseProvider` 嵌入结构体，改为独立 `ContextualVisionProvider` 接口 + 调用方类型断言回退。更简洁，不侵入现有 provider。
- 上下文提取：输出格式为 `[user] ...\n[assistant] ...`，递归收集 `tool_result` 内嵌套 text（深度限制 16），跳过所有 image 块。最后一条含 image 的 user 消息整体跳过。
- 配置：支持环境变量 `BLIND_VISION_CONTEXT_ROUNDS` / `BLIND_VISION_CONTEXT_MAX_CHARS` 覆盖 YAML。`context_rounds < 0` 在 handler 中规范化为 0（禁用）。

**新增日志 stage**：
- `context_extract_complete`：status=ok/empty/disabled，含 message_count、context_chars、duration_ms
- `vision_call_dispatch`：method=DescribeImageWithContext/DescribeImage，含 has_context、context_text
- `prompt_built`（client 层）：system_prompt、context_block_text、desc_instruction，打印实际传给 vision 模型的完整文本 prompt

**Bug 修复**：maxChars 截断逻辑 `i > 0` 保护的是最旧消息而非最新，改为 `i < len(formatted)-1`。回归测试 `TestExtract_MaxChars_TwoMessages_BothExceed`。

**测试覆盖**：9 个单元测试（nil/empty、rounds=0、单轮、多轮截断、maxChars 截断、双消息超限、跳过 image、嵌套 tool_result、最后 user 含 image 跳过）。

## 10. 环境事实

- **Go 版本**：go 1.26.5（`go.mod` 声明；用了 loopvar 修复，但代码里仍显式 `i, blk := i, blk` 防御）
- **Windows PowerShell**：环境变量用 `$env:VAR = "value"` 语法；bash-style `VAR=value command` 会抛 CommandNotFoundException
- **Trae IDE 沙箱限制**：在 `%AppData%\blind-llm-eyes\` 下创建 `*.tmp`（pidfile 的 os.CreateTemp）会被沙箱拒，导致 `status`/`stop` 无法工作；**在独立 PowerShell 终端运行无此问题**；前台 `start` 不受影响
- **CC Switch 代理模式会截断图片数据**（316→188 bytes），必须用 `ANTHROPIC_BASE_URL` 直连；`setup`/`connect` 均显式用直连不经过代理
- **cc-switch SQLite**：`~/.cc-switch/cc-switch.db`（Windows = `%USERPROFILE%\.cc-switch\`），表 `providers(app_type, settings_config JSON)`；modernc.org/sqlite 纯 Go 无 CGO，goreleaser 三平台可交叉编译
- **cc-switch DB 锁定**：GUI 运行中时 SQLite 文件被独占锁，导入逻辑自动 fallback 到 temp copy 后打开读；仍失败 → setup 回退手填（best-effort）
- **MiMo 端点**：`https://api.xiaomimimo.com/anthropic`（Anthropic Messages API，支持 `thinking.type: disabled`），不是 `/v1`（OpenAI Chat Completions）
- **MiMo 默认思考模式**：不关闭 `thinking.type` 会生成 ~2257 chars 隐藏 `reasoning_content`，导致 body_read 23.5s；关闭后 4.2s（-82%）
- **模型名 `[1m]` 后缀**：cc-switch UI 给部分模型加长度尾标（`deepseek-chat[1m]` / `mimo-v2.5[1M]`），DeepSeek 上游会拒；`modelutil.SanitizeModel` 在转发层和导入层双保险剥离
- **pidfile 路径**：`os.UserConfigDir()/blind-llm-eyes/pidfile.json` = Windows 下 `%AppData%\blind-llm-eyes\pidfile.json`，字段 pid/addr/token/startedAt；admin token 仅存此处，不持久化到别处
- **Claude Code settings.json 路径**：`~/.claude/settings.json` = Windows `%USERPROFILE%\.claude\settings.json`，备份 `~/.claude/.bak-before-connect`（首次 connect 创建，重复 connect 永不覆盖）
- **真实 API key** 在 `config.yaml`（gitignore），不在代码里
- **API key 注入优先级**：`config.yaml upstream.api_key` > 环境变量 `BLIND_UPSTREAM_API_KEY` / `BLIND_VISION_API_KEY`；UpstreamAPIKey 已配置时客户端 Authorization 会被显式剥离（防泄露）
- **pre-commit hook** 已配置（`git config core.hooksPath .githooks`），扫描 `sk-[A-Za-z0-9]{20,}` 阻止 key 泄露；config-*-test.yaml 和 `.trae/` 已在 .gitignore
- **git 历史已清理**：旧 commit 中的 API key 已用 `git filter-repo` 替换为 `REDACTED_USE_ENV_VAR`
- **goreleaser 交叉编译**：CGO_ENABLED=0（全依赖纯 Go，含 modernc.org/sqlite），6 目标：linux/darwin/windows × amd64/arm64；ldflags 注入 `buildinfo.Version`
- **发布阻塞**：GITHUB_TOKEN 未设置时无法上传 GitHub Release Assets；本地 `goreleaser build --snapshot` 验证构建全通过

## 11. 相关文档

| 文件 | 内容 |
|------|------|
| [CHANGELOG.md](./CHANGELOG.md) | **变更日志**：v1.0.1 + v1.1.0-dev（Tier 2 M0+M1.A+M1.B+perf 优化）完整变更摘要、Commit 列表、测试覆盖表、已知限制 |
| [RELEASE_NOTES-v1.0.0.md](./RELEASE_NOTES-v1.0.0.md) | **v1.0.0 发布说明**：7 大新功能 / 改进 / 5 E2E 用例表格 / 发布基础设施 / 升级指引 / 12 提交完整列表 / 已知限制 / 验证总结 |
| [RELEASE_NOTES-v1.0.1.md](./RELEASE_NOTES-v1.0.1.md) | **v1.0.1 GA 发布说明**（当前 GA 版本） |
| [CONCURRENCY_DESIGN.md](./CONCURRENCY_DESIGN.md) | errgroup 并发优化 + concurrency_limit 设计文档 |
| [docs/superpowers/specs/2026-08-12-multi-vision-provider-pool-design.md](./docs/superpowers/specs/2026-08-12-multi-vision-provider-pool-design.md) | P3 多 Provider 池设计规格文档 |
| [docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md](./docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md) | **Tier 2 设计规格**：两层 LRU+SQLite 缓存 + Qwen-VL 预设架构，383 行 |
| [docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md](./docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md) | **Tier 2 实施计划**：22 个 TDD 任务（M0+M1.A/B/C/D+M2/M3），1947 行；当前已完成 Task 1-12 + perf 补丁，Task 13-22 待办 |
| [docs/superpowers/plans/2026-08-16-sqlite-inmemory-counter.md](./docs/superpowers/plans/2026-08-16-sqlite-inmemory-counter.md) | **perf 优化任务**：内存计数器消除每次 Put 的 COUNT(*) + CAS 防并发 evict 风暴补丁 |
| [vision-fallback-architecture.md](./vision-fallback-architecture.md) | 原始架构设计 v1 |
| [vision-fallback-notes.md](./vision-fallback-notes.md) | 决策时间线 + 调研记录 |
| [SMOKE_TEST_REPORT.md](./SMOKE_TEST_REPORT.md) | 生产冒烟测试报告（20 样本延迟数据 + AIMD 决策） |
| [CONTEXT.md](./CONTEXT.md) | 项目上下文 |
| [.trae/documents/plan-onboarding-productize.md](./.trae/documents/plan-onboarding-productize.md) | **产品化 onboarding 实施计划（已全部完成 2026-08-13）**：T1-T11 实际完成情况与 commit 对照表，验证总结（注：.trae/ 在 gitignore，仅本地可见） |
| [.goreleaser.yaml](./.goreleaser.yaml) | goreleaser 6 平台交叉编译 + ldflags 配置 |
| [.github/workflows/release.yml](./.github/workflows/release.yml) | GitHub Release CI：`v*` tag push → goreleaser release |
| [Makefile](./Makefile) | Dev targets：`test` / `vet` / `build` / `snapshot` / `goreleaser-check` / `release` / `clean` |
