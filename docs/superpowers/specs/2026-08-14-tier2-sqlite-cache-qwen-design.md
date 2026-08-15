# 设计文档 — Tier 2: 持久化 SQLite 缓存 + Qwen-VL 预设

> 版本: v1.1.0 设计稿
> 日期: 2026-08-14
> 范围: A1 两层 LRU+SQLite 持久化缓存 + C4 DashScope/Qwen-VL 预设
> 状态: 待评审

---

## 1. 总览

### 1.1 目标

本设计覆盖 v1.1.0 的两项核心功能（用户确认的最小聚焦范围）：

| ID | 目标 | 验收标准 |
|---|---|---|
| A1 | 视觉描述跨重启存活 | 同一图片，重启（关闭并重开缓存层）后再次请求 `cached=1`、`vision_calls=0` |
| C4 | 国内视觉 provider 开箱即用 | `doctor` + `DescribeImage` 对 DashScope Qwen-VL 通过 |

### 1.2 非目标（明确排除）

以下 Tier 2 候选项**不在本次范围**：

- A3 OpenAI Chat Completions 输入兼容
- 图像预处理（>2048px 缩放 / >20MB 压缩）
- Retroactive healing narrative
- 百度千帆 ERNIE-VL（需 OAuth2.0 自定义适配器，本次跳过）
- 豆包 / Volcengine Ark 预设
- GLM-4V 深化（已在 v1.0.1 以 `glm_free` 完成）

### 1.3 背景与约束

- **现有缓存**：`cache.LRU` 是具体类型（非接口），`proxy/handler.go` 的 `Cache` 字段直接声明为 `*cache.LRU`。要做两层缓存必须先抽出 `cache.Cache` 接口。
- **CGO 硬约束**：必须用纯 Go 的 `modernc.org/sqlite`，保 `CGO_ENABLED=0`（项目硬约束，goreleaser 跨平台交叉编译依赖）。
- **Trae 沙箱**：v1.0.1 已修复 `os.CreateTemp` 在受保护目录被阻断的问题（改固定名 temp+rename）。SQLite 的 `modernc` 驱动直接创建 DB 文件（非 CreateTemp），WAL 创建 `cache.db-wal`/`cache.db-shm` 也是直接文件创建，沙箱下安全。
- **DashScope 兼容性**（联网研究确认）：阿里云百炼 Qwen-VL 完全 OpenAI 兼容——`https://dashscope.aliyuncs.com/compatible-mode/v1`，标准 `Authorization: Bearer <key>`，`image_url` 格式。现有 `OpenAIClient` 直接可用，无需新客户端。

### 1.4 依赖

- 新增 Go 依赖：`modernc.org/sqlite`（纯 Go SQLite 驱动，经 `database/sql` 注册）。
- 无其他新依赖。

---

## 2. §1 — 架构、接口与数据流

### 2.1 接口抽象（解除 handler 与 `*LRU` 的耦合）

[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go) 当前把 `Cache` 字段声明为具体类型 `*cache.LRU`。引入接口：

```go
// cache/cache.go
package cache

// Cache 是 hash→描述 缓存的抽象接口。
// 现有 *LRU 零改动即满足此签名；新增 *TwoTier 也实现此接口。
type Cache interface {
    Get(key string) (string, bool)
    Put(key, value string)
}
```

- `LRU`（现有）已满足签名，**零改动**即实现 `Cache` 接口。
- `handler.go` 的 `HandlerDeps.Cache` 字段类型从 `*cache.LRU` 改为 `cache.Cache`。
- `NewHandler` 中的 `if deps.Cache == nil { deps.Cache = cache.NewLRU(1000) }` 兜底逻辑保留（默认仍为 LRU）。

### 2.2 包结构

```
cache/
├── cache.go        # 新增：Cache 接口 + 哨兵错误
├── hash.go         # 现有，不动
├── lru.go          # 现有，不动（已满足 Cache 接口）
├── lru_test.go     # 现有
├── sqlite.go       # 新增：SQLite 冷层
├── twotier.go      # 新增：复合层，实现 Cache
├── sqlite_test.go  # 新增
└── twotier_test.go # 新增
```

`modernc.org/sqlite` 通过 `database/sql` 的驱动注册（`import _ "modernc.org/sqlite"`），驱动名 `sqlite`。

### 2.3 数据流

- **`Get(hash)`**：
  1. 查 LRU → 命中返回（µs 级，不碰 SQLite）。
  2. 未命中查 SQLite → 命中则回填 LRU 并 `UPDATE cache SET last_accessed=? WHERE hash=?`，返回。
  3. 都没命中返回 `("", false)`。

- **`Put(hash, desc)`**：
  1. 写 LRU（同步，可能淘汰最旧内存条目）。
  2. `UPSERT` 进 SQLite（WAL，快）+ 更新 `last_accessed`。
  3. SQLite 若 `COUNT(*) > sqlite_max_entries`，按 `last_accessed` 删最旧一批降至 90% 上限（批量删摊销开销）。
  4. 若 `sqlite_ttl > 0`，同批删除 `created_at < now-ttl` 的过期条目。

- **淘汰分治**：LRU 淘汰只丢内存（条目仍在 SQLite，下次 Get 可回填）；SQLite 淘汰才是真删除。

### 2.4 并发模型

- `LRU` 已有 `sync.Mutex`，自身线程安全。
- SQLite 用单 `*sql.DB`（`database/sql` 内部连接池线程安全）+ WAL（读不阻塞写）。
- `TwoTier` 自身加一把 `sync.Mutex`，**仅串行化 Get 的"查冷层→回填热层"复合步骤**，避免惊群重复回填同一 key。Put 不需该锁（LRU/SQLite 各自线程安全，重复写只是幂等覆盖）。

---

## 3. §2 — SQLite Schema、配置与损坏恢复

### 3.1 Schema

```sql
CREATE TABLE IF NOT EXISTS cache (
    hash           TEXT PRIMARY KEY,           -- 来自 cache.HashFromRawBytes（sha256 前16字节 URL-safe base64）
    description    TEXT NOT NULL,
    size_bytes     INTEGER NOT NULL,            -- 描述文本字节数，供 stats 子命令统计
    created_at     INTEGER NOT NULL,            -- unix ms
    last_accessed  INTEGER NOT NULL             -- unix ms，SQLite 层 LRU 淘汰依据
);
CREATE INDEX IF NOT EXISTS idx_cache_last_accessed ON cache(last_accessed);
```

- `hash` 主键与现有 `cache.HashFromRawBytes` 输出格式一致，无需改 hash 逻辑。
- `last_accessed` 索引支撑按访问时间淘汰的 `DELETE ... ORDER BY last_accessed LIMIT n`。

**关键 SQL 操作**：

```sql
-- UPSERT（Put）
INSERT INTO cache(hash, description, size_bytes, created_at, last_accessed)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(hash) DO UPDATE SET
    description   = excluded.description,
    size_bytes    = excluded.size_bytes,
    last_accessed = excluded.last_accessed;

-- Get 命中时更新访问时间
UPDATE cache SET last_accessed = ? WHERE hash = ?;

-- 容量淘汰（Put 后若超上限，删最旧 10%）
DELETE FROM cache WHERE hash IN (
    SELECT hash FROM cache ORDER BY last_accessed ASC LIMIT ?
);

-- TTL 淘汰（若配置 TTL）
DELETE FROM cache WHERE created_at < ?;
```

### 3.2 配置扩展（[config/loader.go](file:///d:/Code/new-api-contrib/config/loader.go) 的 `CacheCfg`）

现有 `CacheCfg` 只有 `MaxEntries int`。扩展为：

```go
type CacheCfg struct {
    MaxEntries       int           `yaml:"max_entries"`         // LRU 热层容量，默认 500（现状不变）
    Type             string        `yaml:"type"`                // "lru"(默认/空) | "twotier"
    DBPath           string        `yaml:"db_path"`             // SQLite 路径；type=twotier 时空→默认 ./cache.db
    SqliteMaxEntries int           `yaml:"sqlite_max_entries"`  // SQLite 容量上限，默认 10000（≤0→10000）；不限请设极大值或靠 TTL
    SqliteTTLStr     string        `yaml:"sqlite_ttl"`          // 持续时间如 "720h"(30天)；空/0=不限 TTL
    SqliteTTL        time.Duration `yaml:"-"`
}
```

**默认值**（在 `Load` 里处理）：

| 字段 | 默认值 | 说明 |
|---|---|---|
| `Type` | `"lru"`（空时） | 向后兼容：现有 config 不含此字段 → 行为不变 |
| `MaxEntries` | `500`（≤0 时） | 现状不变 |
| `DBPath` | `./cache.db`（type=twotier 且空时） | 与 pidfile 同 cwd 约定，规避跨目录沙箱写问题 |
| `SqliteMaxEntries` | `10000`（≤0 时） | SQLite 层容量；要"不限"设极大值如 999999999 |
| `SqliteTTL` | `0`（空=不限） | 解析 `SqliteTTLStr`；0 表示不做 TTL 淘汰 |

**向后兼容**：现有 `config.yaml` 不含新字段 → `Type` 默认 `lru` → 行为与 v1.0.1 完全一致。持久化是 **opt-in**。

### 3.3 WAL 与 PRAGMA（`Open` 时设置）

```sql
PRAGMA journal_mode=WAL;     -- 读不阻塞写，崩溃后自恢复
PRAGMA synchronous=NORMAL;   -- WAL 下 NORMAL 安全且快（无需每写 fsync）
PRAGMA busy_timeout=5000;    -- 5s 等锁，规避偶发 SQLITE_BUSY
```

### 3.4 损坏恢复

`Open` 时跑 `PRAGMA integrity_check;`：

1. **返回 `ok`** → 正常使用。
2. **返回非 `ok`** → 记 `WARN` 日志（含 integrity_check 输出）→ 关句柄 → 删除 `cache.db` + `cache.db-wal` + `cache.db-shm` 三个文件 → 重建空库 → 继续。**冷启动丢描述但不阻断服务**（持久化是增强，不是关键路径）。
3. **文件根本无法打开**（权限/路径错误）→ `Open` 返回 error，`main.go` 降级为 **LRU-only**：记 `WARN`，注入纯 LRU。proxy 照常运行，仅失去持久化。

### 3.5 `main.go` 装配逻辑

```go
var c cache.Cache
switch cfg.Cache.Type {
case "twotier":
    sqlc, err := cache.OpenSQLite(cfg.Cache.DBPath, cfg.Cache.SqliteMaxEntries, cfg.Cache.SqliteTTL, logger)
    if err != nil {
        logger.Warn("sqlite open failed, falling back to LRU-only", "err", err)
        c = cache.NewLRU(cfg.Cache.MaxEntries)
    } else {
        c = cache.NewTwoTier(cfg.Cache.MaxEntries, sqlc, logger)
    }
default: // "lru" 或空
    c = cache.NewLRU(cfg.Cache.MaxEntries)
}
```

---

## 4. §3 — 缓存 CLI 子命令、Qwen 预设、测试与上线

### 4.1 缓存 CLI 子命令

在 [cli/cli.go](file:///d:/Code/new-api-contrib/cli/cli.go) 的 `Run` switch 加 `case "cache"` → `runCache(rest, stdin, stdout, stderr)`，再按 `rest[0]` 分发 4 个子命令。它们读 config 拿 `db_path`，直接开 SQLite（WAL 允许与运行中的 proxy 并发访问）。

新增文件 [cli/cache.go](file:///d:/Code/new-api-contrib/cli/cache.go)：

| 子命令 | 作用 | 退出码 |
|---|---|---|
| `cache stats` | 条目数、总字节数、最旧/最新 `last_accessed`、DB 文件大小、WAL 模式 | 0 成功 / 1 无 DB |
| `cache list [-limit N]` | 列 N 条（默认 20）：hash 前缀 + 描述预览(前 60 字) + `last_accessed` | 0 / 1 无 DB |
| `cache clear` | 删全部行（交互确认），返回删除条数 | 0 / 1 无 DB / 2 取消 |
| `cache path` | 打印 DB 路径 + 是否存在 + 归属 `type`(lru/twotier) | 0 |

**无 DB 场景**：`type=lru`（无持久层）时，除 `path` 外的子命令报 `"cache is LRU-only, no persistent store"` 并退 1。`path` 始终可用（报告当前 type 与为何无 DB）。

**`printUsage` 更新**：在 [cli/cli.go](file:///d:/Code/new-api-contrib/cli/cli.go) 的命令列表加 `cache  Manage persistent cache (stats/list/clear/path)`。

### 4.2 Qwen 预设

#### 4.2.1 provider 类型（[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go)）

仿 `glm_free` 模式，加 `qwen` 类型：

```go
const (
    QwenBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
    QwenModel   = "qwen-vl-plus"   // 通用视觉；文档备选 qwen3-vl-flash（有免费额度）
)

// BuildProvider 里：
func BuildProvider(pc config.ProviderCfg, logger *slog.Logger) (VisionProvider, error) {
    // qwen 自动填充 base_url + model（同 glm_free 模式）
    if pc.Type == "qwen" {
        if pc.BaseURL == "" { pc.BaseURL = QwenBaseURL }
        if pc.Model == ""    { pc.Model = QwenModel }
    }
    // ... 既有 glm_free 自动填充 ...
    // ... 字段校验 ...
    switch pc.Type {
    case "mimo":
        return NewClient(...)
    case "openai_compatible", "glm_free", "qwen":
        return NewOpenAIClient(...)
    default:
        return nil, fmt.Errorf(...)
    }
}
```

DashScope 是 OpenAI 兼容（`/chat/completions` + `image_url` + Bearer 鉴权），现有 `OpenAIClient` 直接可用，**无需新客户端**。

#### 4.2.2 setup 向导（[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go)）

在 provider 选项加 Qwen，并修正预设写出方式。当前 `generateConfigYAML` 只写 `vision:`（MiMo/Anthropic）——对 Qwen（OpenAI 兼容）是错的。

**针对性修正**：选预设时改写 `vision_providers:` 带 `type`。这是 brainstorming 流程认可的"对影响当前工作的既有问题做针对性改进"。

选项重排：

```
Vision provider options:
  1. GLM-4V-Flash (FREE — zero cost, get a key at https://open.bigmodel.cn)
  2. Qwen-VL (DashScope — China first choice, get a key at https://bailian.console.aliyun.com)
  3. MiMo / other Anthropic-compatible (manual)
  4. OpenAI-compatible (manual)
Choose vision provider [1/2/3/4, default=1]:
```

选预设时写出（`generateConfigYAML` 扩展支持 `vision_providers`）：

```yaml
# 选 Qwen 预设时写出：
vision_providers:
  - name: qwen
    type: qwen
    api_key: <用户填>
    # base_url/model 自动填，可省略
```

GLM 预设同步改为写 `vision_providers` + `type: glm_free`，与 Qwen 预设输出格式统一（v1.0.1 中 GLM 预设写 `vision:` 块，由 `BuildSingleProvider` 处理；改写 `vision_providers` 后改走 `BuildProvider` 的 OpenAI 客户端路径——两条路径是否都已实测可用需在 M0 确认；若 `vision:` 路径已实测通过，则 GLM 预设保持写 `vision:`，仅 Qwen 预设新增 `vision_providers`，避免扰动已发布行为）。手动 MiMo（选项 3）仍写 `vision:` 单块。

### 4.3 测试方案

#### 4.3.1 单元测试

- **`cache/sqlite_test.go`**：Open、Put/Get 往返、UPSERT 覆盖、`last_accessed` 更新、超容量按 LRU 删最旧、TTL 过期、`integrity_check` 通过、**损坏恢复**（往 db 文件写垃圾字节 → Open 重建空库 → 服务不中断）。
- **`cache/twotier_test.go`**：Get LRU 命中(不读 SQLite)、Get LRU 未命中→SQLite 命中→回填、Get 双未命中、Put 同写两层、LRU 淘汰后 SQLite 仍有该条目、SQLite 淘汰有界、并发 Get 同 key 不惊群（`TwoTier` 互斥锁）、`-race` 通过。
- **`cache/cache_test.go`**（接口）：`LRU` 与 `TwoTier` 都满足 `Cache` 接口的编译期断言（`var _ Cache = (*LRU)(nil)` / `(*TwoTier)(nil)`）+ 既有 LRU 行为不回归。
- **`vision/provider_test.go`**：`qwen` 类型自动填 base_url+model、派发到 `OpenAIClient`、api_key 必填校验。
- **`cli/cache_test.go`**：stats/list/clear/path 对临时 DB 的行为；`type=lru` 时的友好报错。

#### 4.3.2 集成 / E2E

- **`test/e2e_test.go` 新增 Q1**（核心验收）：同一图片 → 首次 `vision_calls=1`；**关闭并重开 `TwoTier`**（模拟重启）→ 再次请求 `cached=1`、`vision_calls=0`。这是"描述跨重启存活"的验收。

#### 4.3.3 CI 门禁

- `go test -race -count=1 ./...` 全绿。
- `CGO_ENABLED=0 go build ./...` 通过（保纯 Go，项目硬约束）。
- `go vet ./...` 零警告。
- `goreleaser check` 合法。

### 4.4 上线 / 向后兼容

- `Cache.Type` 默认 `lru` → **现有 config.yaml 零改动、零行为变化**。持久化 opt-in。
- 损坏/打不开 → 降级 LRU-only + `WARN`，proxy 照常跑（持久化是增强非关键路径）。
- 无需迁移：空配置→LRU；`type: twotier`→首次运行自动建库。
- 版本：`v1.1.0`（minor，新功能，无破坏性变更）。goreleaser tag 驱动同 v1.0.1。
- setup 向导默认仍推 GLM 免费（零成本门槛最低），Qwen 作为"国内首选、需阿里云 key"的次选项。

---

## 5. 附录

### 5.1 配置示例

```yaml
# 启用持久化缓存
cache:
  type: twotier              # lru(默认) | twotier
  max_entries: 500           # LRU 热层容量
  db_path: ./cache.db        # 空则默认 ./cache.db
  sqlite_max_entries: 10000  # SQLite 容量上限；0=不限
  sqlite_ttl: "720h"          # 30 天 TTL；空=不限

# Qwen 预设（setup 向导生成或手填）
vision_providers:
  - name: qwen
    type: qwen
    api_key: ${DASHSCOPE_API_KEY}
    # base_url/model 自动填
```

### 5.2 新增/变更文件清单

| 文件 | 动作 | 说明 |
|---|---|---|
| `cache/cache.go` | 新增 | `Cache` 接口 + 哨兵错误 |
| `cache/sqlite.go` | 新增 | SQLite 冷层：Open/Get/Put/eviction/integrity_check |
| `cache/twotier.go` | 新增 | 复合层，实现 `Cache` |
| `cache/*_test.go` | 新增 | 单元测试 |
| `proxy/handler.go` | 改 | `Cache` 字段类型 `*LRU` → `Cache` |
| `config/loader.go` | 改 | `CacheCfg` 扩展 + 默认值 |
| `main.go` | 改 | 装配逻辑（switch type） |
| `vision/provider.go` | 改 | `qwen` 类型 + 常量 |
| `cli/cli.go` | 改 | `cache` 子命令分发 + usage |
| `cli/cache.go` | 新增 | 4 个 cache 子命令实现 |
| `cli/setup.go` | 改 | Qwen 选项 + 预设写 `vision_providers` |
| `cli/*_test.go` | 改/增 | 测试 |
| `test/e2e_test.go` | 改 | Q1 跨重启存活 |
| `go.mod` | 改 | 加 `modernc.org/sqlite` |
| `config.example.yaml` | 改 | 文档化新字段 |

### 5.3 里程碑（与产品策略 §9.3 对齐）

| 里程碑 | 退出标准 |
|---|---|
| M0: 接口锁定 | `cache.Cache` 接口合并；`handler.go` 改用接口；`qwen` 类型 stub；`cache` 子命令 stub；`make test` PASS |
| M1: 功能完成 | A1（TwoTier + SQLite + 4 CLI 子命令 + 损坏恢复）；C4（Qwen 预设 + setup 集成）；e2e Q1 PASS |
| M2: RC1 | Tag `v1.1.0-rc1`；goreleaser 6 平台构建；手工 QA（DashScope 真实 key doctor+DescribeImage）；双语发布说明草稿 |
| M3: GA | Tag `v1.1.0`；`go install @v1.1.0` 可解析；E2E 全绿；GitHub Release 发布 |

### 5.4 风险与缓解

| 风险 | 缓解 |
|---|---|
| `modernc.org/sqlite` 意外引入 CGO | CI 每 PR 跑 `CGO_ENABLED=0 go build ./...`；`go vet` 不会抓 CGO，需显式门禁 |
| SQLite 损坏导致 proxy 启动失败 | `integrity_check` + 删库重建 + 降级 LRU-only，绝不阻断服务 |
| Trae 沙箱阻断 DB 文件创建 | 默认 `./cache.db`（cwd，与 pidfile 同约定）；modernc 直接创建文件非 CreateTemp，沙箱安全 |
| setup 预设改写方式扰动既有 GLM 用户 | M0 先实测 v1.0.1 的 `vision:` 路径是否可用：若可用则 GLM 预设保持原样，仅新增 Qwen `vision_providers`；若不可用才改写 GLM。既有手填 `vision:` 块的用户任何情况都不受影响（BuildSingleProvider 路径不动） |
| DSH 在 v1.1.0 开发期发布图片委托 PR | 立即收窄范围到 A1+C4 并在 24h 内发布；评估转向 structured JSON（v2.0） |
