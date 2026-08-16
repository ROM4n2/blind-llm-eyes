# Changelog

## [Unreleased] — Tier 2 (v1.1.0-dev, M0 + M1.A)

**迭代范围**：2026-08-15，共 12 个提交（`0758431..1d69b5e`），对应
[spec](docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md)
和
[plan](docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md)
的 Task 1-11（M0 接口锁定 + M1.A SQLite 缓存）。

**迭代目标**：为 v1.1.0 引入两个 opt-in 特性的骨架与核心实现——两层
LRU+SQLite 持久化缓存（描述文本跨进程重启存活）和 DashScope Qwen-VL
视觉 provider 预设（国内用户友好）。默认行为保留 v1.0.1
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

### Refactor

#### 缓存接口抽象（M0, Task 1）

- 引入 `cache.Cache` 接口 (`cache/cache.go`)：
  `Get(key string) (string, bool)` + `Put(key, value string)`
- `proxy/handler.go` 的 `Cache` 字段类型从 `*LRU` 改为 `cache.Cache`，
  解耦 handler 与具体缓存实现，支持 LRU/TwoTier 无缝切换
- 通过编译期断言 `var _ Cache = (*LRU)(nil)` / `(*TwoTier)(nil)`
  保证实现一致性

### Documentation

- **设计文档**
  (`docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md`)：
  383 行，覆盖架构、接口、schema、配置、降级策略、测试方案，明确排除
  Qianfan OAuth、Doubao、图像预处理、OpenAI Chat Completions 输入兼容
- **实现计划**
  (`docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md`)：
  1947 行，22 个 TDD 任务，每任务含完整代码与预期测试输出

### Test Coverage

| 变更文件 | 测试文件 | 覆盖范围 |
|---|---|---|
| `cache/sqlite.go` | `cache/sqlite_test.go` | Open / idempotent reopen / corruption recovery / Get-Put / eviction by count / eviction by TTL |
| `cache/twotier.go` | `cache/twotier_test.go` | Get 回填 / Put 双写 / 50-goroutine 防惊群 |
| `config/loader.go` | `config/loader_test.go` | 默认值 / twotier 解析 / bad-type 拒绝 |
| `vision/provider.go` | `vision/provider_test.go` | Qwen auto-fill / 用户 override 优先 / 空 api_key 报错 |
| `cli/cache.go` + `cli/cli.go` | `cli/cli_test.go` | `TestRun_Routing` 覆盖 cache no-args / unknown / stats stub |
| `proxy/handler.go` | 现有 proxy 测试 | 接口改动不破坏现有行为 |
| `main.go` | — | 装配逻辑无单元测试（Go 惯例由 e2e 覆盖，规划在 Task 18-20） |

- `go vet ./...` 通过
- `go build ./...` 通过
- `go test -race -count=1 ./...` 全部 PASS（13 个包，race-clean）

### Known Limitations / Pending

本次迭代为 v1.1.0 的 M0 + M1.A 阶段，以下属后续里程碑（Task 12-22），
**未包含在本次推送中**：

- **M1.B**（Task 12）：Qwen 预设的 setup 向导集成
- **M1.C**（Task 13-17）：`cache stats/list/clear/path` 子命令实现
  （当前仅 stub）
- **M1.D**（Task 18-20）：跨重启缓存存活 e2e 测试 + 用户文档
  （`test/e2e_test.go` 当前 3 处仍用 `cache.NewLRU(10)`，未覆盖
  TwoTier 路径）
- **M2/M3**（Task 21-22）：RC1 + GA 发布

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

---

**版本状态**：v1.1.0-dev（unreleased）。当前 GA 版本仍为
v1.0.1。

## [1.0.1] — 2026-08-15

Tier 1 bugfix release. See release notes for details.
