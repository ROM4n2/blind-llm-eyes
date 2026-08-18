# Changelog

## [v1.3.0] — 2026-08-XX

**迭代范围**：2026-08-18 设计，约 6 周，≈12 commits。Plan A Balanced Ops: 3×P0 + 5×P1，
遵循可观测性与运维增强主题。零破坏性变更；所有 v1.2.0 配置文件无需修改即兼容。

**迭代目标**：为 v1.2.0 引入的安全+分片锁基础补上运维侧短板 —— 配置热加载（SIGHUP + admin HTTP + CLI
三通道）、12 个新 Prometheus 指标覆盖缓存分层命中/漂移、provider 结果/熔断器状态变迁、singleflight
去重效率、请求/响应负载分布、SSE 事件吞吐；debug/pprof 标准 Go 剖析端点配 3 层安全；E2E 测试补 TwoTier
后端与跨重启缓存存活；doctor 新增持久层可写、上游可达、视觉模型存在三项检查；示例配置同步
vision_capable_models / metrics_auth_token / debug_pprof_enabled 字段块和 *int 注释。

### Observability

#### Metrics P0 层 — 缓存分层命中 + 漂移 + provider/CB 事件 (P0)

- `metrics/metrics.go`：新增 `blind_llm_eyes_cache_hits_total`（tier=hot/cold × outcome=hit/miss）、
  `blind_llm_eyes_cache_row_count`（tier=memory/actual）、
  `blind_llm_eyes_cache_drift_pct`（±100% 饱和，60s tick 更新）。
  `cache/lru.go` + `cache/twotier.go` 各读取站点埋点；nil-safe 无指标注入时零额外开销。
- `metrics/metrics.go`：新增 `blind_llm_eyes_provider_calls_total`
  （provider × outcome ∈ success/fail/skip/fallback），埋在 `vision/pool.go CallProvider`
  每个出口分支；新增 `blind_llm_eyes_circuit_breaker_transitions_total`
  （provider × from × to ∈ closed/open/halfopen），埋在 `vision/circuit_breaker.go Transition`
  每次状态机 dispatch。旧同名一标签 CounterVec 与 GaugeVec **保留一版**（迁移窗口）。

#### Metrics P1 层 — singleflight / 图片输入 / 负载分布 / SSE 吞吐 (P1)

- `proxy/handler.go`：singleflight.Do 返回 shared bool → exec/wait 分类 + merged 计数。
- `proxy/handler.go`：image base64 解码后 `len(rawBytes)` → 累加到 `images_bytes_in_total` 按 format 标签。
- `proxy/handler.go` 请求体 size histograms（8 buckets, 1KB–20MB）响应体 size histograms；
  SSE passthrough 写路径扫描每个 `event:` 前缀分类到 message/error/other。

### Reliability

#### 配置热加载 ReloadableConfig (P0)

- `config/loader.go`：新增 `ReloadableConfig`（atomic.Pointer + 进程级互斥 reload）。
  `Load()` 单次原子读取，调用方存为 snapshot 本地变量，单次请求绝不多次 Load 以避免版本交叉。
  `Reload()` 复用 `Load(path)+validate()`；若 NON-RELOADABLE 字段（listen / upstream.base_url /
  upstream.api_key / metrics_auth_token / cache.db_path）有变更 → 拒绝并返回描述性错误，保留旧配置。
- `main.go`：SIGHUP goroutine + `/admin/reload` POST handler（200 全部应用 / 206 pool
  drain-30s 超时强制交换 / 422 验证失败，保留旧配置）。
  `applyReloadSideEffects` 触发三个副作用：slog.LevelVar 级别切换、vision.Pool drain-swap、
  LRU Resize 热层容量调整。
- `cli/reload.go`：新增 `blind-llm-eyes reload` 子命令。读取 pidfile，带 admin token POST
  `/admin/reload`，pretty 打印 JSON 响应。Windows 下 SIGHUP 无法触发，此为主要通道。Unix 上双通道皆可用。

#### E2E 跨重启缓存存活验证 (P1)

- `test/e2e_test.go`：抽取 `setupHandlerBackend(t, cacheImpl)` helper。
  新增 `BasicRewrite_TwoTier`、`SSEPassthrough_TwoTier`、`ModelUtilPassthrough_TwoTier`、
  `CacheSurvivesRestart_TwoTier`。最关键的跨重启测试构造两个独立 handler 生命周期共享同一
  t.TempDir()/shared-cache.db，断言 vision mock 调用计数增量为 0（冷层命中，无需再次调用 vision）。
  Release Gate 要求 `-count=10 -race` 十次全绿（无 flaky）。

#### Doctor 三项新检查 (P1)

- `cli/doctor.go`：在自指 URL 检查后追加三项。
  `db_writable`：OpenSQLite + PRAGMA quick_check + 随机 probe 行写入/删除。type=lru 时 SKIP。
  `upstream_reachable`：5s timeout HEAD 到 `<base_url>/v1/models`；任何 HTTP 响应 PASS；
  TCP/DNS/TLS 连接失败 FAIL。`vision_model_exists`：5s ProviderPing（401 立即 FAIL）；
  成功后可选 `/v1/models`，404 或 model 不在列表 = WARN，退出码仍为 0。

#### debug/pprof 标准 Go 剖析端点 (P0)

- 新增包 `internal/pprofsec`：Wrap(http.Handler, Config) 实现 3 层中间件：
  (1) `Enabled=false` → 返回 404（隐藏端点存在）；
  (2) AuthToken 非空时 `subtle.ConstantTimeCompare` 对 query ?token= 或 X-Metrics-Token；
  (3) AuthToken 为空 → 限制 `net.ParseIP(RemoteAddr host).IsLoopback()`，非回环 403。
  main.go 路由注册 `/debug/pprof/*` 五个端点（Index cmdline profile symbol trace）。
  mutex/block profile 默认关闭（0 采样成本），需调优时通过 env 设置比例。
  7 个单元测试覆盖 3 层每一个失败分支 + IPv6 `[::1]` 通过。

### Compatibility

- 新增 `ReloadableConfig` 类型、配置字段 `debug_pprof_enabled`、`Cache.Close() error` 接口方法
  —— 均为纯**增量**，不改变任何已有导出符号签名（返回值、参数、方法名）。
- 旧 Prometheus 指标名（CacheHitRatio、ProviderCallsTotal 1-label、CircuitBreakerState gauge）
  保留一个大版本迁移窗口，不删除、不重命名、不调整 label 集合。
- v1.2.0 样式配置加载无警告、无行为变化（`debug_pprof_enabled` nil → true 默认，
  `vision_capable_models` 空 → 始终重写，旧行为 100% 等价保留）。

### Engineering

- `config.example.yaml`：同步 vision_capable_models 示例块、metrics_auth_token 块（含 openssl /
  PowerShell token 生成命令）、debug_pprof_enabled 注释块、vision.context_rounds *int 语义修正。
- CI 矩阵 ubuntu job 额外执行 `go test -race -count=10 -run TestE2E_CacheSurvivesRestart_TwoTier ./test`
  作为 flaky gate，失败即 CI 红。
- cache 接口新增 Close()：LRU 空实现；SQLite 真实关闭连接；TwoTier 级联关闭冷层。
  handler shutdown 路径调用（非重启场景不会泄漏连接）。

---

**版本状态**：待发布（等待 Release Gate §0.1.3 逐项打勾后 tag `v1.3.0`）。

## [v1.2.0] — 2026-08-18

**迭代范围**：2026-08-18，共 6 个提交（`9db77b3..4346cf4`），基于只读审计报告
识别的 10 项潜在 Bug 与高危因素 + 聚焦复审发现的 6 项新引入风险，全部修复。

**迭代目标**：通过系统性安全审计与修复，消除 v1.1.0 中存在的安全漏洞（API key
泄露、时序攻击、凭证转发缺陷）、并发瓶颈（全局锁串行化）、跨平台兼容性问题
（Windows PowerShell hook 失效）、自指循环防护缺失，以及可观测性不足（count
漂移不可见）。同时引入 CI 工作流和可选 metrics 认证作为预防性加固。

### Security

#### 日志隐私泄露修复（P0）

- `proxy/handler.go`：日志中的 `context_text` 字段泄露完整对话历史，替换为
  `truncatePreview` 截断的 `context_preview`（80 字节，对齐 UTF-8 rune 边界，
  避免切断中文多字节字符）

#### 时序攻击防护（High）

- `main.go`：`withMetricsAuth` 中间件用 `!=` 比较 token 存在时序攻击风险，
  改用 `crypto/subtle.ConstantTimeCompare` 防止逐字节提取 token

#### Authorization 转发语义修正（High）

- `proxy/handler.go`：`shouldStripHeader` 始终剥离 Authorization 会破坏
  passthrough 模式（`vision_capable_models` 命中 + 无 `UpstreamAPIKey` →
  上游 401）。恢复为仅在 `hasUpstreamKey=true` 时剥离（proxy 注入自己的
  Authorization）；未配置时透明转发客户端凭证给上游

#### 可选 metrics 认证（Fix 9）

- `config/loader.go` + `main.go`：新增 `MetricsAuthToken` 配置（YAML
  `metrics_auth_token` / 环境变量 `BLIND_METRICS_AUTH_TOKEN`），设置后
  `/metrics` 端点通过 `?token=xxx` 或 `X-Metrics-Token` header 鉴权。
  空值 = 无认证（向后兼容）

#### 跨平台 pre-commit hook（Fix 1）

- `.githooks/pre-commit.ps1`：原 bash-only hook 在 Windows 原生 PowerShell
  下静默失败，导致 API key 泄露防护失效。新增 PowerShell 版本复刻相同
  正则扫描逻辑（`sk-`/`AKIA`/`ghp_` 模式）

### Reliability

#### 自指循环防护（Fix 6）

- `proxy/handler.go` + `cli/ccswitch.go` + `cli/doctor.go` + `cli/setup.go`：
  upstream/vision base_url 指向 proxy 自身监听地址会导致无限自转发循环、
  CPU 耗尽、静默失败。新增 `IsSelfReferentialURL()` 基于 `net.ParseIP` 的
  host 规范化，覆盖整个 127.0.0.0/8、IPv6 loopback、unspecified 地址、
  localhost 别名、DNS FQDN 形式（`127.0.0.1.`）。5 层防护：
  1. cc-switch 导入过滤自指 provider
  2. setup 向导拒绝自指手动输入
  3. doctor 报告 FAIL
  4. `NewHandler` 构造期 panic
  5. 运行时返回 508 Loop Detected

#### TwoTier 分片锁重构（Fix 5）

- `cache/twotier.go`：全局 mutex 序列化 cold-layer 查询，不同 key 的并发
  Get 被无谓串行化。重构为 16-shard mutex（FNV-32a hash，内联计算零分配），
  不同 key 可并行查询，同 key 仍由分片锁 + double-check 防止 thundering herd

#### SQLite count 漂移可观测（Fix 7）

- `cache/sqlite.go` + `cli/cache.go`：内存计数器可能与实际行数漂移
  （DELETE 失败或外部写入）。新增 `ActualCount()` 和 `MemoryCount()` 方法，
  `cache stats` CLI 同时显示两者，>5% 偏差输出 WARN

### Compatibility

#### ContextRounds 指针语义（Fix 4）

- `config/loader.go` + `main.go`：`ContextRounds` 从 `int` 改为 `*int`，
  区分 "未设置"（nil → 默认 3）与 "显式 0"（禁用）。原 zero-value 语义模糊：
  写 `context_rounds: 0` 想禁用却得到默认 3

#### normalizeHost FQDN 支持（Low）

- `proxy/handler.go`：`normalizeHost` 缺少 `strings.TrimRight(h, ".")`，
  DNS FQDN 形式（`127.0.0.1.`）绕过自循环检测。已与 `cli/ccswitch.go`
  版本对齐

### Engineering

#### CI 工作流（Fix 11）

- `.github/workflows/ci.yml`：README 声称 `make test` 是 "CI gate" 但
  无 CI 工作流。新增 ubuntu/macos/windows 矩阵运行 `go vet` +
  `go test -race` + `go build`，在每个 push/PR 时触发

#### defaultListen 单一来源（Fix 10）

- `cli/setup.go`：8 处硬编码 `"127.0.0.1:8790"` 提取为 `defaultListen`
  常量，错误消息也使用 `%s` 格式引用，避免将来修改监听地址时消息误导

### 验证

```
go test -race -count=1 ./...  →  12 packages PASS
go vet ./...                   →  clean
go build ./...                 →  clean
```

### 提交清单

| SHA | Type | Summary |
|-----|------|---------|
| `9db77b3` | chore | add cross-platform pre-commit hook and CI workflow |
| `3bf6d3f` | fix(cache) | sharded mutex for TwoTier and count drift observability |
| `c988240` | fix(proxy) | prevent self-referential loops and harden header forwarding |
| `6001950` | feat(config) | pointer-based context_rounds and optional metrics auth |
| `7afc8f6` | fix(proxy) | use constant-time token compare and restore passthrough auth forwarding |
| `4346cf4` | fix(proxy) | harden normalizeHost, truncatePreview, shard hash, and setup messages |
| `a0e492d` | merge | Merge pull request #1 from ROM4n2/fix/audit-high-risk-fixes |

---

**版本状态**：v1.2.0 GA 已发布（2026-08-18）。GitHub Release 6 平台
archives + checksums 已就绪：
https://github.com/ROM4n2/blind-llm-eyes/releases/tag/v1.2.0

## [v1.1.0] — 2026-08-17

**迭代范围**：2026-08-15 ~ 2026-08-17，共 21 个提交（`0758431..3c17240`），对应
[spec](docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md)
和
[plan](docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md)
的 Task 1-22（M0 接口锁定 + M1.A-D SQLite 缓存/Qwen setup/cache CLI/e2e+文档 + M2 RC1 + M3 GA），
外加两个 perf 优化补丁（[内存计数器](docs/superpowers/plans/2026-08-16-sqlite-inmemory-counter.md)）。

**迭代目标**：为 v1.1.0 引入两个 opt-in 特性——两层 LRU+SQLite 持久化缓存
（描述文本跨进程重启存活）和 DashScope Qwen-VL 视觉 provider 预设（国内用户友好），
以及 4 个 `cache` CLI 子命令用于缓存管理。默认行为保留 v1.0.1
（`type=lru`），持久化与 Qwen 预设均需显式启用。

### New Features

#### 持久化 SQLite 缓存（M1.A, Task 4-11）

引入两层缓存架构：LRU 作热层（内存，快速命中），SQLite 作冷层
（持久化，跨重启存活）。

- **SQLite 冷层** (`cache/sqlite.go`)
  - `OpenSQLite` 打开数据库并应用 WAL pragmas
    (`journal_mode=WAL` + `synchronous=NORMAL`)，保证读写并发安全
  - schema：`cache_entries(key TEXT PK, value TEXT, created_at, last_accessed)` + 索引
  - `Get`/`Put` 使用 UPSERT 语义，`Get` 时同步刷新 `last_accessed`
  - 双重淘汰策略：按容量 (`sqlite_max_entries`) LRU 淘汰 + 按 TTL
    (`sqlite_ttl`) 过期清理
  - **损坏自愈**：`PRAGMA integrity_check` 失败或 `applyPragmas` 报
    "file is not a database" 时，自动删除 db/-wal/-shm 文件并重建空库
    ——冷启动丢失描述但不阻塞服务
  - 使用纯 Go 驱动 `modernc.org/sqlite`，保持 `CGO_ENABLED=0` 支持跨平台编译

- **TwoTier 复合缓存** (`cache/twotier.go`)
  - 实现 `cache.Cache` 接口，组合 `*LRU`（热）与 `*SQLite`（冷）
  - `Get` 先查热层，未命中在互斥锁下查冷层并回填热层，双重检查防
    thundering herd
  - `Put` 双写两层（LRU 与 SQLite 各自线程安全，覆盖幂等，无需加锁）
  - 淘汰分治：LRU 淘汰只清内存副本（冷层仍有），SQLite 淘汰才是真删除

- **配置扩展** (`config/loader.go`)
  - `CacheCfg` 新增 `Type`、`DBPath`、`SqliteMaxEntries`、`SqliteTTL` 字段
  - 默认值保留 v1.0.1 行为：`Type=lru`，老配置无需改动
  - 校验：拒绝未知 `Type`、拒绝格式错误的 `sqlite_ttl`、
    `SqliteMaxEntries<=0` 回退到 10000

- **装配与降级** (`main.go`)
  - `type=twotier` 时构建 TwoTier；SQLite 打开失败时降级到 LRU-only
    并打 WARN 日志
  - `type=lru`（默认）保持纯内存模式，行为与 v1.0.1 一致

#### Qwen-VL 视觉 Provider 预设（M0, Task 2）

- **预设支持** (`vision/provider.go`)
  - 新增 `qwen` provider 类型，对接 DashScope（阿里云百炼）OpenAI 兼容接口
  - 自动填充 `base_url=https://dashscope.aliyuncs.com/compatible-mode/v1`
    和 `model=qwen-vl-plus`，用户只需配置 `api_key`
  - 复用 `*OpenAIClient`（与 `glm_free` 同路径），不走 Anthropic Messages 协议
- **配置白名单** (`config/loader.go`)：`qwen` 类型加入合法 provider 列表，
  默认值在必填校验前应用

#### cache CLI 子命令骨架（M0, Task 3）

- 新增 `blind-llm-eyes cache` 子命令 (`cli/cache.go`)，预留
  `stats` / `list` / `clear` / `path` 四个子命令，当前返回
  "not implemented yet"。具体实现在 M1.C（Task 13-17）

#### Qwen-VL setup 向导预设 + GLM 客户端路径修正（M1.B, Task 12）

- **setup 向导 Qwen 选项** (`cli/setup.go`)：vision provider 选择菜单
  新增 Qwen-VL（DashScope）作为选项 2，国内用户友好；选项重排为
  1=GLM / 2=Qwen / 3=MiMo 手动 / 4=OpenAI 手动
- **预设统一输出 vision_providers** (`cli/setup.go`)：GLM 和 Qwen 预设
  现在写出 `vision_providers + type` 而非 `vision:` 单块，走
  `BuildProvider -> OpenAIClient`
- **GLM 404 bug 修正**：之前 GLM 预设把
  `https://open.bigmodel.cn/api/paas/v4`（OpenAI 兼容接口）填入
  `vision.BaseURL`，走 MiMo Client 打 `{base}/v1/messages` 返回 404；
  现在走 `OpenAIClient` 打 `/chat/completions`，路径正确
- **手动模式保留**：选项 3/4 仍走 `vision:` 单块（MiMo Client），
  presetType 为空时才提示 base_url/api_key/model 手动输入

#### cache CLI 子命令实现（M1.C, Task 13-17）

- **4 个子命令** (`cli/cache.go`)：`path` / `stats` / `list` / `clear`，
  替换 M0 阶段的 stub
- **path**：打印缓存类型、db 路径、文件是否存在（twotier 才检查）
- **stats**：查询条目数、总字节数、最早/最晚访问时间、db 文件大小、
  实际 journal_mode（非硬编码）
- **list**：按 `last_accessed DESC` 列出条目，hash 截断 12 字符、
  desc 截断 60 字符 + `…`，`-limit N`（默认 20）
- **clear**：`DELETE FROM cache`，交互 `y/N` 确认或 `-yes` 跳过
- **改进**：每个子命令支持 `-config` flag（与 doctor/connect 一致）；
  `openCacheDB` 设置 `busy_timeout=5000`，代理运行中也能操作
- **测试**：17 个新用例覆盖 LRU-only 拒绝、twotier 有/无 db、空 db、
  desc 截断、clear 确认/取消/`-yes`、未知子命令、`-config` flag

#### 跨重启缓存存活 E2E + 文档（M1.D, Task 18-20）

- **E2E 测试** (`test/e2e_test.go`)：`TestE2E_CacheSurvivesRestart`
  验证 SQLite 冷层跨重启存活——冷启动 TwoTier 发请求（vision 调用 1x，
  header "rewritten"）→ 关闭 SQLite（WAL checkpoint）→ 重开同 db 路径
  → 同图请求命中冷层缓存（vision 不再调用，header "cached"）
- **config.example.yaml 文档化**：cache 段补全 `type` / `db_path` /
  `sqlite_max_entries` / `sqlite_ttl` 注释；vision_providers 段新增
  Qwen-VL 示例
- **README 更新**（en + zh）：功能列表（缓存持久化 + Qwen 预设 +
  多 provider 池）、配置参考表（5 个 cache 新字段）、架构图（TwoTier +
  四预设）、新增"缓存管理"小节、CLI 子命令 8→9、限制段更新
- **RELEASE_NOTES-v1.1.0**（en + zh）：完整发版说明，覆盖两层缓存
  架构、Qwen 预设、cache CLI、perf 补丁、向后兼容、21 commit 列表、
  升级指南

### Refactor

#### 缓存接口抽象（M0, Task 1）

- 引入 `cache.Cache` 接口 (`cache/cache.go`)：
  `Get(key string) (string, bool)` + `Put(key, value string)`
- `proxy/handler.go` 的 `Cache` 字段类型从 `*LRU` 改为 `cache.Cache`，
  解耦 handler 与具体缓存实现，支持 LRU/TwoTier 无缝切换
- 通过编译期断言 `var _ Cache = (*LRU)(nil)` / `(*TwoTier)(nil)`
  保证实现一致性

### Perf Optimization

#### SQLite 内存计数器消除每次 Put 的全表 COUNT（d707a2e）

- `evictIfNeeded` 原先每次 `Put` 末尾执行 `SELECT COUNT(*) FROM cache`，
  SQLite 的 `COUNT(*)` 是 O(N) 全表扫描，大库时阻塞写线程
- 改为 `atomic.Int64` 内存计数器：启动 `initCount` 一次 COUNT 初始化，
  运行期零 COUNT；`Put` 用 `INSERT OR IGNORE` + `UPDATE` 两步区分新增/更新，
  新增时 `Add(1)`；evict DELETE 后 `Add(-deleted)`
- 性能改善：10k 行库每次 Put 从 ~1ms 降到 ~0（100x），1M 行从 ~100ms 降到 ~0
- 风险：count 可能短暂漂移，但 SQLite 单写者 + WAL 原子性保证关键不变式

#### CAS 防护并发 evict 风暴（656bab4）

- 内存计数器引入新风险：突发并发 Put 时 N 个 goroutine 同时越过
  `maxEntries` 阈值，每个都执行 `DELETE LIMIT del`，cache 被过度清空
- 加 `evicting atomic.Bool` CAS 闸门：`CompareAndSwap(false, true)` 失败
  的 goroutine 直接返回，只允许一个 evict 同时进行
- 权衡：count 在突发窗口内可能短暂超 maxEntries，但下一次 Put 触发 CAS
  后收敛（用短暂超量换取避免风暴）
- 并发测试 `TestSQLite_EvictNoThunderingHerd`：50 goroutine + barrier 同步，
  `-race -count=10` 反复运行 0 失败

### Documentation

- **设计文档**
  (`docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md`)：
  383 行，覆盖架构、接口、schema、配置、降级策略、测试方案，明确排除
  Qianfan OAuth、Doubao、图像预处理、OpenAI Chat Completions 输入兼容
- **实现计划**
  (`docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md`)：
  1947 行，22 个 TDD 任务，每任务含完整代码与预期测试输出
- **perf 优化任务**
  (`docs/superpowers/plans/2026-08-16-sqlite-inmemory-counter.md`)：
  内存计数器消除每次 Put 的 COUNT(*)，含 CAS 防风暴补丁与并发测试方案

### Test Coverage

| 变更文件 | 测试文件 | 覆盖范围 |
|---|---|---|
| `cache/sqlite.go` | `cache/sqlite_test.go` | Open / idempotent reopen / corruption recovery / Get-Put / eviction by count / eviction by TTL / **内存计数器 Put/evict/rebuild** / **CAS 防风暴 50-goroutine** |
| `cache/twotier.go` | `cache/twotier_test.go` | Get 回填 / Put 双写 / 50-goroutine 防惊群 |
| `config/loader.go` | `config/loader_test.go` | 默认值 / twotier 解析 / bad-type 拒绝 |
| `vision/provider.go` | `vision/provider_test.go` | Qwen auto-fill / 用户 override 优先 / 空 api_key 报错 |
| `cli/cache.go` + `cli/cli.go` | `cli/cli_test.go` + `cli/cache_test.go` | `TestRun_Routing` 覆盖 cache no-args / unknown；**17 个 cache 测试**：path LRU/twotier、stats LRU/twotier/empty、list 基础/截断/LRU、clear `-yes`/取消/确认/LRU、dispatch、`-config` flag |
| `cli/setup.go` | `cli/setup_test.go` | 手动 MiMo / doctor 失败保存/取消 / connect / 默认值 / **GLM 预设 vision_providers** / **Qwen 预设** / **GLM 统一输出** |
| `proxy/handler.go` | 现有 proxy 测试 | 接口改动不破坏现有行为 |
| `main.go` + `cache/twotier.go` | `test/e2e_test.go` | **`TestE2E_CacheSurvivesRestart`**：TwoTier 跨重启缓存存活（WAL checkpoint → 重开 → 冷层命中） |

- `go vet ./...` 通过
- `go build ./...` 通过
- `go test -race -count=1 ./...` 全部 PASS（13 个包，race-clean）

### Known Limitations / Pending

本次迭代覆盖 v1.1.0 全部里程碑（M0 + M1.A-D + M2 + M3），无遗留待办。

- ~~**M1.B**（Task 12）：Qwen 预设的 setup 向导集成~~ ✓ 已完成（`de514b1`）
- ~~**M1.C**（Task 13-17）：`cache stats/list/clear/path` 子命令实现~~ ✓ 已完成（`7da5013`）
- ~~**M1.D**（Task 18-20）：跨重启缓存存活 e2e 测试 + 用户文档~~ ✓ 已完成（`b00e907` + `188145e` + `3c17240`）
- ~~**M2/M3**（Task 21-22）：RC1 + GA 发布~~ ✓ 已完成（tag `v1.1.0-rc1` + `v1.1.0`，GitHub Release 6 平台产物已发布）

### 环境配置变更（本会话）

- **origin URL 改为 SSH**：`git@github.com:ROM4n2/blind-llm-eyes.git`
  （原 HTTPS 在本网络环境 21s 超时，SSH 端口 22 畅通）
- **永久环境变量**（User 级）：`GIT_SSH=C:\WINDOWS\System32\OpenSSH\ssh.exe`
  + `HOME=C:\Users\Haoyu`，让 git 用 Windows OpenSSH 而非 msys2 ssh
  找到 ssh-agent 的 ED25519 key

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
| `20732ab` | docs | add changelog for tier2 m0+m1.a iteration |
| `d707a2e` | perf(cache) | avoid per-put count(*) via in-memory counter |
| `656bab4` | fix(cache) | add cas guard to prevent concurrent evict storm |
| `de514b1` | feat(cli) | add qwen preset and unify preset output to vision_providers |
| `41e9443` | docs | update handoff and changelog for tier2 v1.1.0-dev handover |
| `7da5013` | feat(cli) | implement cache path/stats/list/clear subcommands |
| `b00e907` | test(e2e) | add cache survives restart scenario |
| `188145e` | docs | document cache and qwen options in config example |
| `3c17240` | docs | add v1.1.0 release notes and readme updates |

---

**版本状态**：v1.1.0 GA 已发布（2026-08-17）。GitHub Release 6 平台
archives + checksums 已就绪：
https://github.com/ROM4n2/blind-llm-eyes/releases/tag/v1.1.0

## [1.0.1] — 2026-08-15

Tier 1 bugfix release. See release notes for details.
