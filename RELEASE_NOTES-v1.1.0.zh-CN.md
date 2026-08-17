# 发版说明 — v1.1.0

> ← 返回 [README](README.zh-CN.md) · [v1.0.1 说明](RELEASE_NOTES-v1.0.1.zh-CN.md)

**blind-llm-eyes** — 给纯文本 LLM 装上眼睛。

发版日期：2026-08-17
范围：自 `v1.0.1` 起 21 个提交 — Tier 2（M0 + M1.A-D + perf 补丁）

---

## 概览

v1.1.0 在 v1.0.x 基础上新增两个 opt-in 特性：**两层持久化缓存**（LRU +
SQLite），让图片描述跨代理重启存活；以及 **DashScope Qwen-VL 视觉
provider 预设**，方便阿里云生态用户。两者均为显式启用 — 默认行为与
v1.0.1 完全一致（`cache.type: lru`，单 vision provider）。

新增 `cache` CLI 子命令，提供四个操作（`path` / `stats` / `list` /
`clear`），用于查看和管理持久化存储。

无破坏性变更。现有 `config.yaml` 完全兼容；新字段均为可选，带合理默认值。

---

## 新特性

### 1. 两层持久化缓存（LRU + SQLite）

[cache/sqlite.go](file:///d:/Code/new-api-contrib/cache/sqlite.go) ·
[cache/twotier.go](file:///d:/Code/new-api-contrib/cache/twotier.go) ·
[main.go](file:///d:/Code/new-api-contrib/main.go)

默认的内存 LRU 缓存在重启后丢失所有描述。对于每天处理几十张截图的个人
代理，重启后重新描述每张图既浪费视觉 API 配额，又增加每图约 8s 延迟。

新增 `twotier` 缓存类型在 LRU 热层后增加 SQLite 冷层：

- **热层** — 内存 LRU，与 v1.0.x 一致。亚微秒命中，无 I/O。
- **冷层** — SQLite（纯 Go 驱动 `modernc.org/sqlite`，无 CGO）。WAL
  日志模式保证并发读写安全。描述跨进程重启、崩溃、重新部署存活。
- **Get 流程** — 热层未命中 → 互斥锁下查冷层 → 回填热层 → 双重检查
  防 thundering herd。
- **Put 流程** — 双写两层（幂等，无锁）。
- **淘汰分治** — LRU 淘汰只清内存副本；SQLite 淘汰（按
  `sqlite_max_entries` 容量或 `sqlite_ttl` 年龄）才是真删除。
- **损坏自愈** — `PRAGMA integrity_check` 失败或 "file is not a
  database" 错误时，自动删除 db/-wal/-shm 文件并重建空库。冷启动丢失
  描述但绝不阻塞服务。
- **优雅降级** — 启动时 SQLite 打开失败，代理降级到 LRU-only 并打
  WARN 日志，而非崩溃。

**配置方式：**

```yaml
cache:
  type: twotier              # opt-in；默认 lru（不变）
  max_entries: 500           # LRU 热层容量
  db_path: ./cache.db        # SQLite 路径；默认 ./cache.db
  sqlite_max_entries: 10000  # 冷层容量上限
  sqlite_ttl: "720h"         # 30 天 TTL；空 = 不限
```

### 2. DashScope Qwen-VL 视觉 provider 预设

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go) ·
[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go)

新增 `qwen` provider 类型，自动填充 DashScope（阿里云百炼）OpenAI 兼容
端点和模型。用户只需从 `https://dashscope.aliyun.com` 获取 `api_key`：

```yaml
vision_providers:
  - name: qwen
    type: qwen                    # 自动填充 base_url + model
    priority: 1
    api_key: "sk-qwen-placeholder"
    # base_url: https://dashscope.aliyuncs.com/compatible-mode/v1  （自动）
    # model: qwen-vl-plus                                         （自动）
    # model: qwen-vl-max           # 可覆盖为更强模型
```

`setup` 向导现在与 GLM-4V-Flash、MiMo 并列提供 Qwen-VL 选项。两个预设
均写入带正确 `type` 字段的 `vision_providers[]`，生成的配置无需手动
编辑即可使用。

### 3. `cache` CLI 子命令

[cli/cache.go](file:///d:/Code/new-api-contrib/cli/cache.go)

四个子命令用于查看和管理持久化缓存：

| 子命令 | 行为 |
|---|---|
| `cache path` | 显示缓存类型、db 路径、db 文件是否存在 |
| `cache stats` | 条目数、总字节数、最早/最晚访问、db 文件大小、journal 模式 |
| `cache list` | 列出条目（12 字符哈希前缀 + 60 字符描述预览），`-limit N` |
| `cache clear` | 删除所有条目，交互 `y/N` 确认或 `-yes` 跳过 |

每个子命令支持 `-config <路径>`（默认 `config.yaml`）。`stats` / `list`
/ `clear` 在 `cache.type` 为 `lru` 时退出码 1（无持久存储）。CLI 打开
SQLite 时设置 `busy_timeout=5000`，代理运行中也能操作。

### 4. 性能优化：内存计数器 + CAS 淘汰守卫

[cache/sqlite.go](file:///d:/Code/new-api-contrib/cache/sqlite.go)

SQLite 缓存附带两个性能补丁：

- **内存计数器** — `atomic.Int64` 跟踪条目数，避免每次 `Put` 都跑
  `O(N) SELECT COUNT(*)`。计数器在打开时初始化一次，之后原子更新；
  淘汰检查读计数器，不查表。
- **CAS 淘汰守卫** — `atomic.Bool` CAS 确保同一时刻只有一个 goroutine
  执行 `evictIfNeeded`，防止大量 `Put` 同时触达容量边界时引发并发
  淘汰风暴。

---

## 向后兼容

- **默认缓存类型为 `lru`** — 不配置 `cache.type` 的现有配置行为与
  v1.0.1 完全一致。零行为变化。
- **SQLite 为 opt-in** — 仅在显式设置 `type: twotier` 时才创建
  `cache.db` 文件。
- **Qwen 预设为增量** — 单 provider 的 `vision:` 字段仍然有效；
  `vision_providers` 仅在配置时使用。
- **无新增必填配置字段** — 所有新字段均有默认值。

---

## 本次发版提交

```text
b00e907 test(e2e): add cache survives restart scenario
7da5013 feat(cli): implement cache path/stats/list/clear subcommands
41e9443 docs: update handoff and changelog for tier2 v1.1.0-dev handover
de514b1 feat(cli): add qwen preset and unify preset output to vision_providers
656bab4 fix(cache): add cas guard to prevent concurrent evict storm
d707a2e perf(cache): avoid per-put count(*) via in-memory counter
20732ab docs: add changelog for tier2 m0+m1.a iteration
1d69b5e feat(main): wire two-tier cache with lru-only fallback
1991fa1 feat(config): extend cachecfg with type dbpath and ttl
cd32794 feat(cache): add two-tier lru+sqlite composite cache
b993018 feat(cache): add sqlite integrity check and corruption recovery
4a9e9ff feat(cache): add sqlite lru and ttl eviction
8dc4afd feat(cache): add sqlite get/put with upsert and last_accessed
5d193c7 feat(cache): add sqlite open with schema and wal pragmas
eb1197d feat(cli): add cache subcommand stub and usage
245487e feat(vision): add qwen provider type for DashScope Qwen-VL
e239a96 refactor(cache): introduce Cache interface to decouple handler
941e426 docs: add tier2 implementation plan
0758431 docs: add tier2 sqlite cache and qwen-vl preset design
```

---

## 验证

- `go test -race -count=1 ./...` — 13 个包全绿
- `go vet ./...` — 无问题
- `go build ./...` — 无问题（CGO_ENABLED=0）
- E2E 跨重启测试（`TestE2E_CacheSurvivesRestart`）— 验证描述跨模拟
  代理重启存活（关闭 SQLite → 重开同 db 路径 → 同图命中冷层缓存 →
  视觉不再被调用）

---

## 升级指南

v1.0.1 的直接替换。启用新特性：

- **持久化缓存** — 在 `config.yaml` 中添加：
  ```yaml
  cache:
    type: twotier
    db_path: ./cache.db           # 可选，默认 ./cache.db
    sqlite_max_entries: 10000     # 可选，默认 10000
    sqlite_ttl: "720h"            # 可选，30 天 TTL
  ```
  `cache.db` 文件首次运行时创建。查看或清理：
  `blind-llm-eyes cache stats` / `cache list` / `cache clear`。

- **Qwen-VL provider** — 重跑 `blind-llm-eyes setup` 选 Qwen 选项，
  或在 `vision_providers` 中添加 `type: qwen` 条目，只需填 `api_key`
  （base_url 和 model 自动填充）。

- **缓存管理** — `blind-llm-eyes cache path|stats|list|clear`。
  用法详见 `blind-llm-eyes cache`。
