# Changelog

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
