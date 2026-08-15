# Tier 2: SQLite 持久化缓存 + Qwen-VL 预设 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 blind-llm-eyes 加两层 LRU+SQLite 持久化缓存（描述跨重启存活）和 DashScope Qwen-VL 视觉 provider 预设（OpenAI 兼容、零额外客户端）。

**Architecture:** 引入 `cache.Cache` 接口解除 handler 对 `*LRU` 的耦合；`TwoTier` 组合 `*LRU`(热)+`*SQLite`(冷)，Get 先热后冷回填、Put 同写两层(WAL)，淘汰分治。Qwen 走现有 `OpenAIClient`，setup 向导选预设时改写 `vision_providers`+`type`（同时修正 GLM 预设客户端路径）。纯 Go `modernc.org/sqlite` 保 `CGO_ENABLED=0`。

**Tech Stack:** Go 1.24+、`modernc.org/sqlite`（纯 Go SQLite）、`database/sql`、`gopkg.in/yaml.v3`、`errgroup`（已用）、`log/slog`。

**Spec:** [docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md](file:///d:/Code/new-api-contrib/docs/superpowers/specs/2026-08-14-tier2-sqlite-cache-qwen-design.md)

---

## 文件结构

| 文件 | 动作 | 职责 |
|---|---|---|
| `cache/cache.go` | 新增 | `Cache` 接口 + 编译期断言 |
| `cache/sqlite.go` | 新增 | SQLite 冷层：Open/Get/Put/eviction/integrity/corruption-recovery |
| `cache/twotier.go` | 新增 | 复合层，实现 `Cache` |
| `cache/*_test.go` | 新增 | 单元测试（TDD） |
| `proxy/handler.go` | 改 L37, L87-88 | `Cache` 字段 `*cache.LRU`→`cache.Cache` |
| `config/loader.go` | 改 L64-66, L144-146, L217-302 | `CacheCfg` 扩展 + 默认值 + `qwen` 类型校验 |
| `main.go` | 改 L69, L131-146 | 装配逻辑（switch type）+ 降级 |
| `vision/provider.go` | 改 L43-97 | `qwen` 类型 + `QwenBaseURL`/`QwenModel` 常量 |
| `cli/cli.go` | 改 L28-53, L68-80 | `cache` 子命令分发 + usage |
| `cli/cache.go` | 新增 | stats/list/clear/path 4 个子命令 |
| `cli/cache_test.go` | 新增 | 子命令测试 |
| `cli/setup.go` | 改 L104-152, L216-233 | Qwen 选项 + `vision_providers` 写出 + GLM 统一 |
| `test/e2e_test.go` | 改 | Q1 跨重启存活 |
| `config.example.yaml` | 改 | 文档化 `cache.*` 新字段 + `qwen` 示例 |
| `go.mod` / `go.sum` | 改 | 加 `modernc.org/sqlite` |

---

## M0 — 接口锁定（编译期安全，行为不变）

### Task 1: 抽出 `cache.Cache` 接口，handler 改用接口

**Files:**
- Create: `cache/cache.go`
- Modify: `proxy/handler.go:37`, `proxy/handler.go:87-88`

- [ ] **Step 1: 写 `cache/cache.go`**

```go
package cache

// Cache 是 hash→描述 缓存的抽象接口。
// *LRU（内存）与 *TwoTier（LRU+SQLite）都实现此接口。
// handler 只依赖此接口，便于注入不同后端与 mock。
type Cache interface {
	Get(key string) (string, bool)
	Put(key, value string)
}

// 编译期断言：现有 LRU 与新增 TwoTier 都满足 Cache 接口。
var (
	_ Cache = (*LRU)(nil)
	_ Cache = (*TwoTier)(nil)
)
```

注意：`_ Cache = (*TwoTier)(nil)` 此时编译会失败（TwoTier 尚未定义）。先注释掉 TwoTier 这行，在 Task 9 取消注释。

- [ ] **Step 2: 改 [proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go) L37 字段类型**

把 `Cache *cache.LRU` 改为 `Cache cache.Cache`：

```go
type HandlerDeps struct {
	UpstreamBaseURL     string
	UpstreamAPIKey      string
	VisionProvider      vision.VisionProvider
	Cache               cache.Cache   // 接口：*LRU 或 *TwoTier
	FailOpen            bool
	// ... 其余字段不变
}
```

- [ ] **Step 3: 改 [proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go) L87-88 兜底（保持不变即可，NewLRU 返回 *LRU 满足接口）**

```go
if deps.Cache == nil {
	deps.Cache = cache.NewLRU(1000)
}
```

无需改动（`*LRU` 已满足 `Cache`）。

- [ ] **Step 4: 验证编译 + 全量测试不回归**

Run: `go build ./... && go test -race -count=1 ./...`
Expected: 全绿（接口改动对现有 `*LRU` 透明）。

- [ ] **Step 5: 提交**

```bash
git add cache/cache.go proxy/handler.go
git commit -m "refactor(cache): introduce Cache interface to decouple handler"
```

---

### Task 2: `qwen` provider 类型 stub（派发占位，先编译通过）

**Files:**
- Modify: `vision/provider.go:43-97`, `config/loader.go:235-238`

- [ ] **Step 1: 在 [vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go) L50 后加 Qwen 常量**

```go
const (
	GLMFreeBaseURL = "https://open.bigmodel.cn/api/paas/v4"
	GLMFreeModel   = "glm-4v-flash"

	// Qwen-VL DashScope (Aliyun) OpenAI-compatible defaults.
	// base_url 是百炼 compatible-mode；model qwen-vl-plus 为通用视觉模型。
	// 用户需在 https://bailian.console.aliyun.com 申请 API key。
	QwenBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	QwenModel   = "qwen-vl-plus"
)
```

- [ ] **Step 2: 在 `BuildProvider`（L62-97）加 `qwen` 自动填充与派发**

在 `glm_free` 自动填充块后加 `qwen`：

```go
func BuildProvider(pc config.ProviderCfg, logger *slog.Logger) (VisionProvider, error) {
	// glm_free / qwen 自动填充 base_url 和 model；其他类型需全部填写。
	if pc.Type == "glm_free" {
		if pc.BaseURL == "" {
			pc.BaseURL = GLMFreeBaseURL
		}
		if pc.Model == "" {
			pc.Model = GLMFreeModel
		}
	}
	if pc.Type == "qwen" {
		if pc.BaseURL == "" {
			pc.BaseURL = QwenBaseURL
		}
		if pc.Model == "" {
			pc.Model = QwenModel
		}
	}
	// ... 既有 base_url/api_key/model 校验不变 ...
	switch pc.Type {
	case "mimo":
		return NewClient(/* ... 不变 ... */), nil
	case "openai_compatible", "glm_free", "qwen":
		return NewOpenAIClient(/* ... 不变 ... */), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q (want \"mimo\", \"openai_compatible\", \"glm_free\", or \"qwen\")", pc.Name, pc.Type)
	}
}
```

- [ ] **Step 3: 在 [config/loader.go](file:///d:/Code/new-api-contrib/config/loader.go) L235-238 把 `qwen` 加入合法 type 白名单**

```go
if p.Type != "mimo" && p.Type != "openai_compatible" && p.Type != "glm_free" && p.Type != "qwen" {
	return nil, fmt.Errorf("vision_providers[%d] %q: type must be \"mimo\", \"openai_compatible\", \"glm_free\", or \"qwen\", got %q",
		i, p.Name, p.Type)
}
```

并在 `glm_free` 自动填充块后加 `qwen` 自动填充（与 BuildProvider 对称，保证 loader 校验前 base_url/model 非空）：

```go
if p.Type == "qwen" {
	if p.BaseURL == "" {
		p.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	if p.Model == "" {
		p.Model = "qwen-vl-plus"
	}
}
```

- [ ] **Step 4: 写失败测试 `vision/provider_test.go` 新增**

```go
func TestBuildProvider_QwenAutoFill(t *testing.T) {
	pc := config.ProviderCfg{
		Name:   "qwen",
		Type:   "qwen",
		APIKey: "ds-key",
		// BaseURL/Model 留空，期望自动填充
		Timeout:            30 * time.Second,
		LargeTimeout:        120 * time.Second,
		LargeImageThreshold: 1_000_000,
		DescriptionCap:     1000,
		SupportedFormats:   []string{"image/png"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := vision.BuildProvider(pc, logger)
	if err != nil {
		t.Fatalf("BuildProvider qwen: %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
	// 派发到 OpenAI 客户端（不能是 MiMo client）
	if _, ok := p.(*OpenAIClient); !ok {
		t.Errorf("qwen must dispatch to *OpenAIClient, got %T", p)
	}
}

func TestBuildProvider_QwenMissingAPIKey(t *testing.T) {
	pc := config.ProviderCfg{Name: "qwen", Type: "qwen"} // 无 api_key
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := vision.BuildProvider(pc, logger); err == nil {
		t.Fatal("expected error for missing api_key")
	}
}
```

若 `provider_test.go` 未导入 `io`/`time`/`slog`，按 goimports 提示补。

- [ ] **Step 5: 跑测试**

Run: `go test -race -count=1 ./vision/`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add vision/provider.go vision/provider_test.go config/loader.go
git commit -m "feat(vision): add qwen provider type for DashScope Qwen-VL"
```

---

### Task 3: `cache` CLI 子命令 stub（分发 + usage，实现后补）

**Files:**
- Modify: `cli/cli.go:28-53`, `cli/cli.go:68-80`
- Create: `cli/cache.go`

- [ ] **Step 1: 写 `cli/cache.go` 占位**

```go
package cli

import (
	"fmt"
	"io"
)

// runCache 实现 `cache` 子命令：管理持久化缓存。
// 子命令：stats / list / clear / path。见 Task 14-18 实现。
func runCache(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCacheUsage(stderr)
		return 2
	}
	switch args[0] {
	case "stats", "list", "clear", "path":
		fmt.Fprintf(stderr, "cache %s: not implemented yet\n", args[0])
		return 2
	default:
		fmt.Fprintf(stderr, "cache: unknown subcommand %q\n", args[0])
		printCacheUsage(stderr)
		return 2
	}
}

func printCacheUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: blind-llm-eyes cache <subcommand>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  stats   Show cache statistics (entries, size, oldest/newest access)")
	fmt.Fprintln(w, "  list    List cache entries (hash prefix + description preview)")
	fmt.Fprintln(w, "  clear   Delete all cache entries")
	fmt.Fprintln(w, "  path    Show the cache database path and type")
}
```

- [ ] **Step 2: 在 [cli/cli.go](file:///d:/Code/new-api-contrib/cli/cli.go) L47 后加分发**

```go
	case "stop":
		return runStop(rest, stdin, stdout, stderr)
	case "cache":
		return runCache(rest, stdin, stdout, stderr)
```

- [ ] **Step 3: 在 `printUsage`（L78 后）加 cache 行**

```go
	fmt.Fprintln(w, "  stop         Stop the running proxy")
	fmt.Fprintln(w, "  cache        Manage persistent cache (stats/list/clear/path)")
	fmt.Fprintln(w, "  version      Print version information")
```

- [ ] **Step 4: 写失败测试 `cli/cli_test.go` 新增用例**

在既有 `TestRun_Routing` 的 cases 切片加：

```go
{"cache no args", []string{"cache"}, 2, "Subcommands", ""},
{"cache unknown", []string{"cache", "frob"}, 2, "unknown subcommand", ""},
{"cache stats stub", []string{"cache", "stats"}, 2, "not implemented yet", ""},
```

- [ ] **Step 5: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestRun_Routing`
Expected: PASS（stub 返回 2）。

- [ ] **Step 6: 提交**

```bash
git add cli/cache.go cli/cli.go cli/cli_test.go
git commit -m "feat(cli): add cache subcommand stub and usage"
```

---

**M0 退出标准：** `go build ./...` 通过；`go test -race -count=1 ./...` 全绿；`CGO_ENABLED=0 go build ./...` 通过。

---

## M1.A — SQLite 持久化缓存

### Task 4: 加 `modernc.org/sqlite` 依赖

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 拉依赖**

Run: `go get modernc.org/sqlite@latest`
Expected: `go.mod` 新增 `modernc.org/sqlite`；`go.sum` 更新。

- [ ] **Step 2: 验证纯 Go（无 CGO）**

Run: `$env:CGO_ENABLED=0; go build ./...`（PowerShell）
Expected: 编译成功（modernc 是纯 Go）。若失败报 CGO，说明拉到了错误包——不要用 `mattn/go-sqlite3`（带 CGO）。

- [ ] **Step 3: 提交**

```bash
git add go.mod go.sum
git commit -m "chore: add pure-go modernc.org/sqlite dependency"
```

---

### Task 5: `cache.SQLite` — Open + schema + PRAGMA + Close

**Files:**
- Create: `cache/sqlite.go`, `cache/sqlite_test.go`

- [ ] **Step 1: 写 `cache/sqlite.go`**

```go
package cache

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，经 database/sql 注册
)

// SQLite 是基于 modernc.org/sqlite 的持久化冷层缓存。
// 线程安全：database/sql 连接池 + WAL（读不阻塞写）。
type SQLite struct {
	db        *sql.DB
	maxEntries int           // 容量上限；<=0 视为极大（不限）
	ttl       time.Duration  // TTL；0=不做 TTL 淘汰
	log       *slog.Logger
}

const (
	sqlCreateTable = `CREATE TABLE IF NOT EXISTS cache (
		hash           TEXT PRIMARY KEY,
		description    TEXT NOT NULL,
		size_bytes     INTEGER NOT NULL,
		created_at     INTEGER NOT NULL,
		last_accessed  INTEGER NOT NULL
	)`
	sqlCreateIndex = `CREATE INDEX IF NOT EXISTS idx_cache_last_accessed ON cache(last_accessed)`
)

// OpenSQLite 打开（必要时创建）SQLite 缓存库，建表 + 设 WAL/PRAGMA +
// 跑 integrity_check。损坏时删库重建（冷启动丢描述但不阻断服务）。
func OpenSQLite(path string, maxEntries int, ttl time.Duration, logger *slog.Logger) (*SQLite, error) {
	if path == "" {
		path = "./cache.db"
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc 单写连接更稳；读并发靠连接池。
	db.SetMaxOpenConns(8)

	s := &SQLite{db: db, maxEntries: maxEntries, ttl: ttl, log: logger}
	if err := s.applyPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.applyCorruptionRecovery(path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) applyPragmas() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

func (s *SQLite) initSchema() error {
	if _, err := s.db.Exec(sqlCreateTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := s.db.Exec(sqlCreateIndex); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }
```

- [ ] **Step 2: 写失败测试 `cache/sqlite_test.go`（Open + Close + schema）**

```go
package cache

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestOpenSQLite_CreatesTableAndIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	s, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	var name string
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='cache'").Scan(&name)
	if err != nil || name != "cache" {
		t.Fatalf("table cache missing: name=%q err=%v", name, err)
	}
	err = s.db.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_cache_last_accessed'").Scan(&name)
	if err != nil || name != "idx_cache_last_accessed" {
		t.Fatalf("index missing: name=%q err=%v", name, err)
	}
}

func TestOpenSQLite_DefaultPath(t *testing.T) {
	// 空路径 → ./cache.db（cwd）。用 t.Chdir 隔离（go 1.24+）。
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := OpenSQLite("", 0, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite empty path: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat("./cache.db"); err != nil {
		t.Fatalf("default db file not created: %v", err)
	}
	_ = time.Second // 防止 go vet 报未用 import（若无需移除）
}
```

若 go 版本 <1.24 无 `t.Chdir`，改用 `os.Chdir` + `defer` 还原。

- [ ] **Step 3: 跑测试验证通过**

Run: `go test -race -count=1 ./cache/ -run TestOpenSQLite`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cache/sqlite.go cache/sqlite_test.go go.mod go.sum
git commit -m "feat(cache): add sqlite open with schema and wal pragmas"
```

---

### Task 6: SQLite `Get` / `Put` / UPSERT / `last_accessed`

**Files:**
- Modify: `cache/sqlite.go`, `cache/sqlite_test.go`

- [ ] **Step 1: 在 `cache/sqlite.go` 加 Get/Put**

```go
const (
	sqlGet = `SELECT description FROM cache WHERE hash = ?`
	sqlTouchAccess = `UPDATE cache SET last_accessed = ? WHERE hash = ?`
	sqlUpsert = `INSERT INTO cache(hash, description, size_bytes, created_at, last_accessed)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			description   = excluded.description,
			size_bytes    = excluded.size_bytes,
			last_accessed = excluded.last_accessed`
)

func (s *SQLite) Get(key string) (string, bool) {
	var desc string
	err := s.db.QueryRow(sqlGet, key).Scan(&desc)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		s.log.Warn("sqlite get", "err", err, "key", key)
		return "", false
	}
	// 更新访问时间（best-effort，失败不影响命中）
	if _, err := s.db.Exec(sqlTouchAccess, nowMillis(), key); err != nil {
		s.log.Warn("sqlite touch access", "err", err, "key", key)
	}
	return desc, true
}

func (s *SQLite) Put(key, value string) {
	now := nowMillis()
	if _, err := s.db.Exec(sqlUpsert, key, value, len(value), now, now); err != nil {
		s.log.Warn("sqlite put", "err", err, "key", key)
	}
	s.evictIfNeeded()
}

func nowMillis() int64 { return time.Now().UnixMilli() }
```

- [ ] **Step 2: 写失败测试**

```go
func TestSQLite_PutGetRoundTrip(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()

	s.Put("h1", "a cat sitting on a mat")
	got, ok := s.Get("h1")
	if !ok || got != "a cat sitting on a mat" {
		t.Fatalf("got (%q,%v), want (\"a cat...\", true)", got, ok)
	}
}

func TestSQLite_GetMiss(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestSQLite_UpsertOverwrites(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	s.Put("h1", "v1")
	s.Put("h1", "v2")
	got, _ := s.Get("h1")
	if got != "v2" {
		t.Fatalf("want v2, got %q", got)
	}
}

func TestSQLite_TouchAccessOnGet(t *testing.T) {
	s := newTestSQLite(t)
	defer s.Close()
	s.Put("h1", "v")
	// 篡改 last_accessed 为很久以前
	_, _ = s.db.Exec("UPDATE cache SET last_accessed = 0 WHERE hash = ?", "h1")
	s.Get("h1")
	var la int64
	_ = s.db.QueryRow("SELECT last_accessed FROM cache WHERE hash = ?", "h1").Scan(&la)
	if la == 0 {
		t.Fatal("Get did not update last_accessed")
	}
}
```

加 helper：

```go
func newTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cache/ -run TestSQLite`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cache/sqlite.go cache/sqlite_test.go
git commit -m "feat(cache): add sqlite get/put with upsert and last_accessed"
```

---

### Task 7: SQLite 淘汰（容量 + TTL）

**Files:**
- Modify: `cache/sqlite.go`, `cache/sqlite_test.go`

- [ ] **Step 1: 在 `cache/sqlite.go` 加 `evictIfNeeded`**

```go
const (
	sqlCount   = `SELECT COUNT(*) FROM cache`
	sqlEvictLRU = `DELETE FROM cache WHERE hash IN (
		SELECT hash FROM cache ORDER BY last_accessed ASC LIMIT ?)`
	sqlEvictTTL = `DELETE FROM cache WHERE created_at < ?`
)

func (s *SQLite) evictIfNeeded() {
	if s.maxEntries > 0 {
		var n int
		if err := s.db.QueryRow(sqlCount).Scan(&n); err != nil {
			s.log.Warn("sqlite count", "err", err)
			return
		}
		if n > s.maxEntries {
			// 删到 90% 上限（批量摊销）
			del := n - s.maxEntries*9/10
			if del < 1 {
				del = 1
			}
			if _, err := s.db.Exec(sqlEvictLRU, del); err != nil {
				s.log.Warn("sqlite evict lru", "err", err)
			}
		}
	}
	if s.ttl > 0 {
		cutoff := nowMillis() - s.ttl.Milliseconds()
		if _, err := s.db.Exec(sqlEvictTTL, cutoff); err != nil {
			s.log.Warn("sqlite evict ttl", "err", err)
		}
	}
}
```

- [ ] **Step 2: 写失败测试**

```go
func TestSQLite_EvictByCount(t *testing.T) {
	s, _ := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 3, 0, discardLogger())
	defer s.Close()
	for i := 0; i < 5; i++ {
		s.Put(fmt.Sprintf("h%d", i), "v")
		time.Sleep(2 * time.Millisecond) // 错开 last_accessed
	}
	var n int
	_ = s.db.QueryRow(sqlCount).Scan(&n)
	if n > 3 {
		t.Fatalf("count %d > maxEntries 3", n)
	}
}

func TestSQLite_EvictByTTL(t *testing.T) {
	s, _ := OpenSQLite(filepath.Join(t.TempDir(), "cache.db"), 0, 1*time.Millisecond, discardLogger())
	defer s.Close()
	s.Put("h1", "v")
	time.Sleep(20 * time.Millisecond)
	s.Put("h2", "v") // 触发淘汰 h1（已过期）
	if _, ok := s.Get("h1"); ok {
		t.Fatal("h1 should have been TTL-evicted")
	}
}
```

需 `import "fmt"`。

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cache/ -run TestSQLite_Evict`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cache/sqlite.go cache/sqlite_test.go
git commit -m "feat(cache): add sqlite lru and ttl eviction"
```

---

### Task 8: SQLite `integrity_check` + 损坏恢复

**Files:**
- Modify: `cache/sqlite.go`, `cache/sqlite_test.go`

- [ ] **Step 1: 在 `cache/sqlite.go` 加 `applyCorruptionRecovery`**

```go
const sqlIntegrityCheck = `PRAGMA integrity_check`

// applyCorruptionRecovery 跑 integrity_check。返回非 ok 则删库重建。
func (s *SQLite) applyCorruptionRecovery(path string) error {
	var result string
	if err := s.db.QueryRow(sqlIntegrityCheck).Scan(&result); err != nil {
		// 连 integrity_check 都跑不了——库严重损坏，重建。
		s.log.Warn("sqlite integrity_check failed, rebuilding", "err", err)
		return s.rebuildDB(path)
	}
	if result != "ok" {
		s.log.Warn("sqlite integrity_check not ok, rebuilding", "result", result)
		return s.rebuildDB(path)
	}
	return nil
}

func (s *SQLite) rebuildDB(path string) error {
	// 关当前句柄，删三个文件，重开。
	s.db.Close()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(f)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("reopen sqlite after rebuild: %w", err)
	}
	db.SetMaxOpenConns(8)
	s.db = db
	if err := s.applyPragmas(); err != nil {
		return err
	}
	return nil
}
```

在 `OpenSQLite` 流程中 `applyPragmas` 之后调 `applyCorruptionRecovery`（已在 Step 1 的 OpenSQLite 写好顺序：pragmas → corruption → schema）。注意 rebuildDB 后 schema 由后续 `initSchema` 重建。

- [ ] **Step 2: 写失败测试（写垃圾字节 → Open 重建）**

```go
func TestOpenSQLite_CorruptionRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	// 写入垃圾字节模拟损坏
	if err := os.WriteFile(path, []byte("not a sqlite database garbage garbage garbage"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	// Open 应检测到损坏并重建（不返回 error）
	s, err := OpenSQLite(path, 10000, 0, discardLogger())
	if err != nil {
		t.Fatalf("OpenSQLite on corrupt db: %v", err)
	}
	defer s.Close()
	// 重建后能正常 Put/Get
	s.Put("h1", "v")
	if got, _ := s.Get("h1"); got != "v" {
		t.Fatalf("after recovery Get=%q want v", got)
	}
}
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cache/ -run TestOpenSQLite_Corruption`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cache/sqlite.go cache/sqlite_test.go
git commit -m "feat(cache): add sqlite integrity check and corruption recovery"
```

---

### Task 9: `cache.TwoTier` 复合层

**Files:**
- Create: `cache/twotier.go`, `cache/twotier_test.go`
- Modify: `cache/cache.go`（取消 Task 1 注释的 TwoTier 断言）

- [ ] **Step 1: 取消 [cache/cache.go](file:///d:/Code/new-api-contrib/cache/cache.go) 中 `_ Cache = (*TwoTier)(nil)` 的注释**

- [ ] **Step 2: 写 `cache/twotier.go`**

```go
package cache

import (
	"log/slog"
	"sync"
)

// TwoTier 是 LRU(热) + SQLite(冷) 的复合缓存。
//   Get: 先查 LRU；未命中查 SQLite，命中则回填 LRU。
//   Put: 同写两层（SQLite 用 WAL，写很快）。
// 淘汰分治：LRU 淘汰只丢内存（条目仍在 SQLite）；SQLite 淘汰才是真删除。
type TwoTier struct {
	hot  *LRU
	cold *SQLite
	log  *slog.Logger

	// mu 串行化 Get 的"查冷层→回填热层"，避免惊群重复回填同一 key。
	// Put 不持此锁（LRU/SQLite 各自线程安全，重复写幂等）。
	mu sync.Mutex
}

func NewTwoTier(lruCap int, cold *SQLite, logger *slog.Logger) *TwoTier {
	if logger == nil {
		logger = slog.Default()
	}
	return &TwoTier{hot: NewLRU(lruCap), cold: cold, log: logger}
}

func (t *TwoTier) Get(key string) (string, bool) {
	// 1) 热层命中
	if v, ok := t.hot.Get(key); ok {
		return v, true
	}
	// 2) 冷层查询 + 回填（串行化避免惊群）
	t.mu.Lock()
	defer t.mu.Unlock()
	// double-check：另一 goroutine 可能刚回填进 LRU
	if v, ok := t.hot.Get(key); ok {
		return v, true
	}
	desc, ok := t.cold.Get(key)
	if !ok {
		return "", false
	}
	t.hot.Put(key, desc)
	return desc, true
}

func (t *TwoTier) Put(key, value string) {
	t.hot.Put(key, value)
	t.cold.Put(key, value) // best-effort，Put 内部已吞错记 WARN
}
```

- [ ] **Step 3: 写失败测试 `cache/twotier_test.go`**

```go
package cache

import (
	"fmt"
	"sync"
	"testing"
)

func TestTwoTier_HotHitNoColdRead(t *testing.T) {
	tt := newTestTwoTier(t)
	defer tt.cold.Close()

	tt.Put("h1", "v1")
	// 篡改 SQLite 让冷层返回不同值，验证热层命中不读冷层
	_, _ = tt.cold.db.Exec("UPDATE cache SET description='STALE' WHERE hash=?", "h1")
	got, _ := tt.Get("h1")
	if got != "v1" {
		t.Fatalf("hot miss: got %q want v1", got)
	}
}

func TestTwoTier_ColdMissBackfills(t *testing.T) {
	tt := newTestTwoTier(t)
	defer tt.cold.Close()
	// 直接写冷层（绕过 LRU）
	tt.cold.Put("h2", "from-cold")
	// 清空热层确保走冷层
	tt.hot = NewLRU(10)
	got, ok := tt.Get("h2")
	if !ok || got != "from-cold" {
		t.Fatalf("cold backfill: got (%q,%v)", got, ok)
	}
	// 回填后热层应命中
	if v, _ := tt.hot.Get("h2"); v != "from-cold" {
		t.Fatalf("backfill to hot failed: %q", v)
	}
}

func TestTwoTier_BothMiss(t *testing.T) {
	tt := newTestTwoTier(t)
	defer tt.cold.Close()
	if _, ok := tt.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestTwoTier_LRUEvictionKeepsCold(t *testing.T) {
	// 热层容量 1：Put h1 后 Put h2 会把 h1 挤出热层，但冷层仍有 h1
	tt, _ := NewTwoTierForTest(1, newTestSQLite(t), discardLogger())
	defer tt.cold.Close()
	tt.Put("h1", "v1")
	tt.Put("h2", "v2")
	// h1 不在热层
	if _, ok := tt.hot.Get("h1"); ok {
		t.Fatal("h1 should be evicted from hot")
	}
	// 但 Get(h1) 应从冷层回填
	if got, _ := tt.Get("h1"); got != "v1" {
		t.Fatalf("cold should still have h1: got %q", got)
	}
}

func TestTwoTier_ConcurrentGetNoThunderingHerd(t *testing.T) {
	tt := newTestTwoTier(t)
	defer tt.cold.Close()
	tt.cold.Put("hX", "vX")
	tt.hot = NewLRU(10) // 强制走冷层

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _ := tt.Get("hX"); got != "vX" {
				t.Errorf("goroutine got %q", got)
			}
		}()
	}
	wg.Wait()
}

func newTestTwoTier(t *testing.T) *TwoTier {
	t.Helper()
	s := newTestSQLite(t)
	tt, _ := NewTwoTierForTest(10, s, discardLogger())
	return tt
}

// NewTwoTierForTest 暴露给测试用（公开以避免循环）。
func NewTwoTierForTest(lruCap int, cold *SQLite, log *slog.Logger) (*TwoTier, error) {
	return NewTwoTier(lruCap, cold, log), nil
}

var _ = fmt.Sprintf // 防 fmt 未用
```

- [ ] **Step 4: 跑测试（含 -race）**

Run: `go test -race -count=1 ./cache/`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add cache/twotier.go cache/twotier_test.go cache/cache.go
git commit -m "feat(cache): add two-tier lru+sqlite composite cache"
```

---

### Task 10: `CacheCfg` 扩展 + 默认值

**Files:**
- Modify: `config/loader.go:64-66`, `config/loader.go:144-146`
- Test: `config/loader_test.go`（若无则新建，但既有项目应有）

- [ ] **Step 1: 改 `CacheCfg`（L64-66）**

```go
type CacheCfg struct {
	MaxEntries       int           `yaml:"max_entries"`         // LRU 热层容量，默认 500
	Type             string        `yaml:"type"`                // "lru"(默认/空) | "twotier"
	DBPath           string        `yaml:"db_path"`             // SQLite 路径；type=twotier 时空→默认 ./cache.db
	SqliteMaxEntries int           `yaml:"sqlite_max_entries"`  // SQLite 容量上限，默认 10000（<=0→10000）
	SqliteTTLStr     string        `yaml:"sqlite_ttl"`          // 持续时间如 "720h"；空/0=不限 TTL
	SqliteTTL        time.Duration `yaml:"-"`
}
```

- [ ] **Step 2: 在 `Load`（L144-146 后）加默认值**

```go
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 500
	}
	if c.Cache.Type == "" {
		c.Cache.Type = "lru"
	}
	if c.Cache.Type != "lru" && c.Cache.Type != "twotier" {
		return nil, fmt.Errorf("cache.type: must be \"lru\" or \"twotier\", got %q", c.Cache.Type)
	}
	if c.Cache.SqliteMaxEntries <= 0 {
		c.Cache.SqliteMaxEntries = 10000
	}
	if c.Cache.SqliteTTLStr != "" {
		d, err := time.ParseDuration(c.Cache.SqliteTTLStr)
		if err != nil {
			return nil, fmt.Errorf("cache.sqlite_ttl: %w", err)
		}
		c.Cache.SqliteTTL = d
	}
```

- [ ] **Step 3: 写失败测试 `config/loader_test.go` 新增**

```go
func TestLoad_CacheDefaults(t *testing.T) {
	path := writeConfigYAML(t, `upstream: {base_url: "http://x"}
vision: {base_url: "http://v", api_key: "k", model: "m"}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Type != "lru" { t.Fatal("default type want lru") }
	if cfg.Cache.SqliteMaxEntries != 10000 { t.Fatal("sqlite_max default") }
	if cfg.Cache.SqliteTTL != 0 { t.Fatal("ttl default 0") }
}

func TestLoad_CacheTwoTier(t *testing.T) {
	path := writeConfigYAML(t, `upstream: {base_url: "http://x"}
vision: {base_url: "http://v", api_key: "k", model: "m"}
cache:
  type: twotier
  sqlite_max_entries: 5000
  sqlite_ttl: "24h"`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Cache.Type != "twotier" { t.Fatal("type") }
	if cfg.Cache.SqliteMaxEntries != 5000 { t.Fatal("max") }
	if cfg.Cache.SqliteTTL != 24*time.Hour { t.Fatal("ttl") }
}

func TestLoad_CacheBadType(t *testing.T) {
	path := writeConfigYAML(t, `upstream: {base_url: "http://x"}
vision: {base_url: "http://v", api_key: "k", model: "m"}
cache: {type: "bogus"}`)
	if _, err := Load(path); err == nil { t.Fatal("want error for bad type") }
}
```

若既有 `writeConfigYAML` helper 不存在，用 `os.WriteFile` 直写临时 yaml 文件路径。

- [ ] **Step 4: 跑测试**

Run: `go test -race -count=1 ./config/`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add config/loader.go config/loader_test.go
git commit -m "feat(config): extend cachecfg with type dbpath and ttl"
```

---

### Task 11: `main.go` 装配 + 降级

**Files:**
- Modify: `main.go:69`, `main.go:131-146`

- [ ] **Step 1: 改 L69 日志（加 cache_type）**

```go
		"cache_max", cfg.Cache.MaxEntries,
		"cache_type", cfg.Cache.Type,
```

- [ ] **Step 2: 在 [main.go](file:///d:/Code/new-api-contrib/main.go) L131 之前构造 cache.Cache**

把 L135 的 `Cache: cache.NewLRU(cfg.Cache.MaxEntries)` 替换为构造逻辑：

```go
	// 构造 cache：type=twotier 用 LRU+SQLite；失败降级 LRU-only（不阻断启动）。
	var cacheBackend cache.Cache
	switch cfg.Cache.Type {
	case "twotier":
		sqlc, err := cache.OpenSQLite(cfg.Cache.DBPath, cfg.Cache.SqliteMaxEntries, cfg.Cache.SqliteTTL, logger)
		if err != nil {
			logger.Warn("sqlite open failed, falling back to LRU-only", "err", err)
			cacheBackend = cache.NewLRU(cfg.Cache.MaxEntries)
		} else {
			cacheBackend = cache.NewTwoTier(cfg.Cache.MaxEntries, sqlc, logger)
			logger.Info("persistent cache enabled",
				"db_path", cfg.Cache.DBPath,
				"sqlite_max_entries", cfg.Cache.SqliteMaxEntries,
				"sqlite_ttl", cfg.Cache.SqliteTTL,
			)
		}
	default:
		cacheBackend = cache.NewLRU(cfg.Cache.MaxEntries)
	}
```

deps 里：

```go
		Cache:               cacheBackend,
```

- [ ] **Step 3: 跑全量构建 + 测试**

Run: `$env:CGO_ENABLED=0; go build ./... ; go test -race -count=1 ./...`
Expected: 通过。无 regression。

- [ ] **Step 4: 提交**

```bash
git add main.go
git commit -m "feat(main): wire two-tier cache with lru-only fallback"
```

---

## M1.B — Qwen 预设 + setup 向导修正

### Task 12: setup 向导 Qwen 选项 + `vision_providers` 写出 + GLM 统一

**Files:**
- Modify: `cli/setup.go:104-152`, `cli/setup.go:216-233`
- Test: `cli/setup_test.go`

> 说明：当前 GLM 预设写 `vision:`（走 `BuildSingleProvider`→MiMo 客户端），但 `open.bigmodel.cn/api/paas/v4` 是 OpenAI 兼容接口，MiMo 客户端会打 `{base}/v1/messages`（404）。本任务把 GLM 与 Qwen 预设统一改写 `vision_providers`+`type`，走 `BuildProvider`→`OpenAIClient`，修正客户端路径错配。

- [ ] **Step 1: 改 [cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go) provider 选择块（L104-123）**

引入 `presetType` 变量跟踪预设；选预设时填 `vision_providers` 所需字段而非 `vision:`：

```go
	// presetType 非空表示用户选了预设（GLM/Qwen），将写出 vision_providers+type。
	// 为空表示手动（走 vision: 单块，MiMo 客户端）。
	var presetType string

	if visionBaseURL == "" {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Vision provider options:")
		fmt.Fprintln(stdout, "  1. GLM-4V-Flash (FREE — zero cost, https://open.bigmodel.cn)")
		fmt.Fprintln(stdout, "  2. Qwen-VL (DashScope — China first, https://bailian.console.aliyun.com)")
		fmt.Fprintln(stdout, "  3. MiMo / other Anthropic-compatible (manual)")
		fmt.Fprintln(stdout, "  4. OpenAI-compatible (manual)")
		choice := prompt("Choose vision provider [1/2/3/4, default=1]: ")
		switch {
		case choice == "" || choice == "1":
			presetType = "glm_free"
			visionAPIKey = prompt("GLM API key: ")
		case choice == "2":
			presetType = "qwen"
			fmt.Fprintln(stdout, "Get an API key at: https://bailian.console.aliyun.com")
			visionAPIKey = prompt("DashScope API key: ")
		case choice == "3":
			// 手动 MiMo，落 vision: 块
		case choice == "4":
			// 手动 OpenAI 兼容，落 vision: 块但用 openai_compatible？setup 无法写 vision_providers 手动。
			// 简化：手动一律落 vision:（MiMo），用户事后可手改。
		}
	}
```

- [ ] **Step 2: 改 cfg 构造（L144-152）让预设走 VisionProviders**

```go
	cfg := &config.Config{
		Listen:   "127.0.0.1:8790",
		Upstream: config.UpstreamCfg{BaseURL: upstreamBaseURL, APIKey: upstreamAPIKey},
		Vision: config.VisionCfg{
			BaseURL: visionBaseURL,
			APIKey:  visionAPIKey,
			Model:   visionModel,
		},
	}
	// 预设模式：doctor 和最终配置都用 vision_providers + type（走对的客户端）
	if presetType != "" {
		cfg.VisionProviders = []config.ProviderCfg{{
			Name:   presetType,
			Type:   presetType,
			APIKey: visionAPIKey,
			// base_url/model 由 loader 默认值自动填（glm_free/qwen）
		}}
		// 清空 vision: 避免两套并存
		cfg.Vision = config.VisionCfg{}
	}
```

注意：doctor 调 `runDoctorCore` 已支持 `cfg.VisionProviders`（doctor.go L70-97 循环 BuildProvider），所以 doctor 会用对的客户端测预设。

- [ ] **Step 3: 改 `generateConfigYAML`（L216-233）写 `vision_providers` when preset**

需要把 presetType 传进去。改函数签名加参数，或在 cfg 上判断。用 cfg 上 `len(VisionProviders)>0` 判断最干净：

```go
func generateConfigYAML(cfg *config.Config) string {
	out := map[string]any{
		"listen": cfg.Listen,
		"upstream": map[string]any{
			"base_url": cfg.Upstream.BaseURL,
			"api_key":  cfg.Upstream.APIKey,
		},
		"log_level": "info",
	}
	if len(cfg.VisionProviders) > 0 {
		// 预设模式：写 vision_providers + type
		ps := make([]map[string]any, 0, len(cfg.VisionProviders))
		for _, p := range cfg.VisionProviders {
			m := map[string]any{
				"name":    p.Name,
				"type":    p.Type,
				"api_key": p.APIKey,
			}
			if p.BaseURL != "" {
				m["base_url"] = p.BaseURL
			}
			if p.Model != "" {
				m["model"] = p.Model
			}
			ps = append(ps, m)
		}
		out["vision_providers"] = ps
	} else {
		out["vision"] = map[string]any{
			"base_url": cfg.Vision.BaseURL,
			"api_key":  cfg.Vision.APIKey,
			"model":    cfg.Vision.Model,
		}
	}
	b, _ := yaml.Marshal(out)
	return string(b)
}
```

- [ ] **Step 4: 写失败测试 `cli/setup_test.go` 新增**

```go
func TestSetup_QwenPresetWritesVisionProviders(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath: "config.yaml",
	}
	// 选 Qwen：第 2 选项，然后填 key
	stdin := strings.NewReader(
		"n\n" +                 // 不 import cc-switch
			"https://up.example\n" + // upstream url
			"up-key\n" +           // upstream key
			"2\n" +                // 选 Qwen
			"ds-key\n" +           // dashscope key
			"n\n")                 // 不 connect
	code := runSetupCore(stdin, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if code != 0 {
		t.Fatalf("setup exited %d", code)
	}
	if !strings.Contains(cfgOut, "vision_providers") {
		t.Errorf("config missing vision_providers:\n%s", cfgOut)
	}
	if !strings.Contains(cfgOut, "type: qwen") {
		t.Errorf("config missing type: qwen:\n%s", cfgOut)
	}
	if strings.Contains(cfgOut, "vision:") {
		t.Errorf("preset should not write vision: block:\n%s", cfgOut)
	}
}

func TestSetup_GLMUnifiedToVisionProviders(t *testing.T) {
	var cfgOut string
	deps := &setupTestDeps{
		writeConfig: func(name, data string) error { cfgOut = data; return nil },
		doctorFunc: func(cfg *config.Config, stdout, stderr io.Writer) int { return 0 },
		configPath: "config.yaml",
	}
	// 选 GLM（默认，直接回车）+ key
	stdin := strings.NewReader("n\nhttps://up.example\nup-key\n\n" + "glm-key\n" + "n\n")
	runSetupCore(stdin, &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if !strings.Contains(cfgOut, "type: glm_free") {
		t.Errorf("GLM preset should write type: glm_free:\n%s", cfgOut)
	}
}
```

确保 setup_test.go 已 `import ("bytes"; "io"; "strings"; "testing")`。

- [ ] **Step 5: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestSetup_`
Expected: PASS。若既有 GLM 测试断言 `vision:` 块，需同步更新为断言 `vision_providers` + `type: glm_free`。

- [ ] **Step 6: 提交**

```bash
git add cli/setup.go cli/setup_test.go
git commit -m "feat(cli): add qwen preset and unify preset output to vision_providers"
```

---

## M1.C — 缓存 CLI 子命令实现

### Task 13: `cache path`（最简，先做）

**Files:**
- Modify: `cli/cache.go`, `cli/cache_test.go`

- [ ] **Step 1: 在 `cli/cache.go` 加 `runCachePath`**

需先能加载 config 拿 `db_path` 与 `type`：

```go
package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ROM4n2/blind-llm-eyes/config"
)

func runCachePath(_ []string, _ io.Reader, stdout, stderr io.Writer) int {
	configPath := "config.yaml"
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 1
	}
	dbPath := cfg.Cache.DBPath
	if dbPath == "" {
		dbPath = "./cache.db"
	}
	fmt.Fprintf(stdout, "type: %s\n", cfg.Cache.Type)
	fmt.Fprintf(stdout, "db_path: %s\n", dbPath)
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stdout, "note: type is not twotier; no persistent store")
		return 0
	}
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stdout, "db_exists: false (%v)\n", err)
	} else {
		fmt.Fprintln(stdout, "db_exists: true")
	}
	return 0
}
```

更新 `runCache` switch：`case "path": return runCachePath(rest, stdin, stdout, stderr)`。

- [ ] **Step 2: 写测试 `cli/cache_test.go`**

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCache_Path_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\n"), 0644)
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 0 { t.Fatalf("code %d %s", code, errB.String()) }
	if !strings.Contains(out.String(), "type: lru") { t.Errorf("out: %s", out.String()) }
	if !strings.Contains(out.String(), "no persistent store") { t.Errorf("out: %s", out.String()) }
}

func TestRunCache_Path_TwoTierNoDB(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\ncache: {type: twotier}\n"), 0644)
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 0 { t.Fatalf("code %d %s", code, errB.String()) }
	if !strings.Contains(out.String(), "type: twotier") { t.Errorf("out: %s", out.String()) }
	if !strings.Contains(out.String(), "db_exists: false") { t.Errorf("out: %s", out.String()) }
}
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestRunCache_Path`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cli/cache.go cli/cache_test.go
git commit -m "feat(cli): implement cache path subcommand"
```

---

### Task 14: `cache stats`

**Files:**
- Modify: `cli/cache.go`, `cli/cache_test.go`

- [ ] **Step 1: 在 `cli/cache.go` 加 `runCacheStats`**

```go
func runCacheStats(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil { return 2 }

	cfg, err := config.Load("config.yaml")
	if err != nil { fmt.Fprintf(stderr, "load config: %v\n", err); return 1 }
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}
	dbPath := cfg.Cache.DBPath
	if dbPath == "" { dbPath = "./cache.db" }
	db, err := sql.Open("sqlite", dbPath)
	if err != nil { fmt.Fprintf(stderr, "open db: %v\n", err); return 1 }
	defer db.Close()

	var n, total int64
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(SUM(size_bytes),0) FROM cache").Scan(&n, &total)
	var oldest, newest sql.NullInt64
	_ = db.QueryRow("SELECT MIN(last_accessed), MAX(last_accessed) FROM cache").Scan(&oldest, &newest)

	fmt.Fprintf(stdout, "entries: %d\n", n)
	fmt.Fprintf(stdout, "total_bytes: %d\n", total)
	if oldest.Valid { fmt.Fprintf(stdout, "oldest_access_ms: %d\n", oldest.Int64) }
	if newest.Valid { fmt.Fprintf(stdout, "newest_access_ms: %d\n", newest.Int64) }
	if fi, err := os.Stat(dbPath); err == nil { fmt.Fprintf(stdout, "db_file_bytes: %d\n", fi.Size()) }
	fmt.Fprintln(stdout, "wal_mode: true")
	return 0
}
```

更新 `runCache` switch：`case "stats": return runCacheStats(rest, stdin, stdout, stderr)`。

- [ ] **Step 2: 写测试**

```go
func TestRunCache_Stats_NoDB_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\n"), 0644)
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"stats"}, nil, &out, &errB)
	if code != 1 { t.Fatalf("want exit 1, got %d", code) }
	if !strings.Contains(errB.String(), "LRU-only") { t.Errorf("err: %s", errB.String()) }
}

func TestRunCache_Stats_TwoTier(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\ncache: {type: twotier, db_path: "+dbPath+"}\n"), 0644)
	// 预填两条
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE cache(hash TEXT PRIMARY KEY, description TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at INTEGER NOT NULL, last_accessed INTEGER NOT NULL)")
	_, _ = db.Exec("INSERT INTO cache VALUES('h1','v1',2,1,1),('h2','v2',2,2,2)")
	db.Close()
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"stats"}, nil, &out, &errB)
	if code != 0 { t.Fatalf("code %d %s", code, errB.String()) }
	if !strings.Contains(out.String(), "entries: 2") { t.Errorf("out: %s", out.String()) }
	if !strings.Contains(out.String(), "total_bytes: 4") { t.Errorf("out: %s", out.String()) }
}
```

需在测试文件 `import "database/sql"`（用 `_ "modernc.org/sqlite"` 驱动注册已在 cache 包，但 cli 测试需自己导入驱动）：

```go
import _ "modernc.org/sqlite"
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestRunCache_Stats`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cli/cache.go cli/cache_test.go
git commit -m "feat(cli): implement cache stats subcommand"
```

---

### Task 15: `cache list`

**Files:**
- Modify: `cli/cache.go`, `cli/cache_test.go`

- [ ] **Step 1: 加 `runCacheList`**

```go
func runCacheList(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 20, "max entries to print")
	if err := fs.Parse(args); err != nil { return 2 }

	cfg, err := config.Load("config.yaml")
	if err != nil { fmt.Fprintf(stderr, "load config: %v\n", err); return 1 }
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}
	dbPath := cfg.Cache.DBPath
	if dbPath == "" { dbPath = "./cache.db" }
	db, err := sql.Open("sqlite", dbPath)
	if err != nil { fmt.Fprintf(stderr, "open db: %v\n", err); return 1 }
	defer db.Close()

	rows, err := db.Query("SELECT hash, description, last_accessed FROM cache ORDER BY last_accessed DESC LIMIT ?", *limit)
	if err != nil { fmt.Fprintf(stderr, "query: %v\n", err); return 1 }
	defer rows.Close()
	for rows.Next() {
		var hash, desc string
		var la int64
		if err := rows.Scan(&hash, &desc, &la); err != nil { continue }
		if len(desc) > 60 { desc = desc[:60] + "…" }
		if len(hash) > 12 { hash = hash[:12] }
		fmt.Fprintf(stdout, "%s  %s  (access_ms=%d)\n", hash, desc, la)
	}
	return 0
}
```

更新 switch：`case "list": return runCacheList(rest, stdin, stdout, stderr)`。

- [ ] **Step 2: 写测试**（复用 Task 14 的预填 db helper）：

```go
func TestRunCache_List(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\ncache: {type: twotier, db_path: "+dbPath+"}\n"), 0644)
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE cache(hash TEXT PRIMARY KEY, description TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at INTEGER NOT NULL, last_accessed INTEGER NOT NULL)")
	_, _ = db.Exec("INSERT INTO cache VALUES('abcdef0123456789','a cat on a mat',10,1,1)")
	db.Close()
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"list", "-limit", "5"}, nil, &out, &errB)
	if code != 0 { t.Fatalf("code %d %s", code, errB.String()) }
	if !strings.Contains(out.String(), "abcdef0123") { t.Errorf("want hash prefix: %s", out.String()) }
	if !strings.Contains(out.String(), "a cat on a mat") { t.Errorf("want desc: %s", out.String()) }
}
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestRunCache_List`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cli/cache.go cli/cache_test.go
git commit -m "feat(cli): implement cache list subcommand"
```

---

### Task 16: `cache clear`

**Files:**
- Modify: `cli/cache.go`, `cli/cache_test.go`

- [ ] **Step 1: 加 `runCacheClear`（交互确认）**

```go
func runCacheClear(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil { return 2 }

	cfg, err := config.Load("config.yaml")
	if err != nil { fmt.Fprintf(stderr, "load config: %v\n", err); return 1 }
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}
	if !*yes {
		fmt.Fprint(stdout, "Delete ALL cache entries? [y/N]: ")
		reader := bufio.NewReader(stdin)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "y" && !strings.EqualFold(line, "yes") {
			fmt.Fprintln(stdout, "cancelled")
			return 2
		}
	}
	dbPath := cfg.Cache.DBPath
	if dbPath == "" { dbPath = "./cache.db" }
	db, err := sql.Open("sqlite", dbPath)
	if err != nil { fmt.Fprintf(stderr, "open db: %v\n", err); return 1 }
	defer db.Close()
	res, err := db.Exec("DELETE FROM cache")
	if err != nil { fmt.Fprintf(stderr, "delete: %v\n", err); return 1 }
	n, _ := res.RowsAffected()
	fmt.Fprintf(stdout, "deleted: %d\n", n)
	return 0
}
```

更新 switch：`case "clear": return runCacheClear(rest, stdin, stdout, stderr)`。需 `import "bufio"; "strings"`（cache.go 已 import strings？检查补）。

- [ ] **Step 2: 写测试**

```go
func TestRunCache_Clear(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\ncache: {type: twotier, db_path: "+dbPath+"}\n"), 0644)
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE cache(hash TEXT PRIMARY KEY, description TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at INTEGER NOT NULL, last_accessed INTEGER NOT NULL)")
	_, _ = db.Exec("INSERT INTO cache VALUES('h1','v',1,1,1),('h2','v',1,1,1)")
	db.Close()
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"clear", "-yes"}, nil, &out, &errB)
	if code != 0 { t.Fatalf("code %d %s", code, errB.String()) }
	if !strings.Contains(out.String(), "deleted: 2") { t.Errorf("out: %s", out.String()) }
}

func TestRunCache_Clear_Cancel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	_ = os.WriteFile(cfgPath, []byte("upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\ncache: {type: twotier, db_path: "+dbPath+"}\n"), 0644)
	db, _ := sql.Open("sqlite", dbPath)
	_, _ = db.Exec("CREATE TABLE cache(hash TEXT PRIMARY KEY, description TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at INTEGER NOT NULL, last_accessed INTEGER NOT NULL)")
	_, _ = db.Exec("INSERT INTO cache VALUES('h1','v',1,1,1)")
	db.Close()
	t.Chdir(dir)
	var out, errB bytes.Buffer
	code := runCache([]string{"clear"}, strings.NewReader("n\n"), &out, &errB)
	if code != 2 { t.Fatalf("want 2, got %d", code) }
	if !strings.Contains(out.String(), "cancelled") { t.Errorf("out: %s", out.String()) }
}
```

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./cli/ -run TestRunCache_Clear`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add cli/cache.go cli/cache_test.go
git commit -m "feat(cli): implement cache clear subcommand"
```

---

### Task 17: 删 `runCache` 的 stub 分支（已全部实现）

**Files:**
- Modify: `cli/cache.go`

- [ ] **Step 1: 确认 `runCache` switch 已无 `not implemented yet` 残留**

把 Task 3 中的：

```go
	case "stats", "list", "clear", "path":
		fmt.Fprintf(stderr, "cache %s: not implemented yet\n", args[0])
		return 2
```

删除——每个子命令已有独立 case。

- [ ] **Step 2: 跑 cli 全量测试**

Run: `go test -race -count=1 ./cli/`
Expected: PASS（含 Task 3 的 stub 测试，此时 stats 不再返回 2——需把 Task 3 测试用例 `{"cache stats stub", ...}` 改为期望 1（LRU-only 无 config 情境）或删除该用例）。

- [ ] **Step 3: 同步更新 `cli/cli_test.go` 的 routing 用例**

把 Task 3 加的 `{"cache stats stub", []string{"cache","stats"}, 2, "not implemented yet", ""}` 删除（已不适用）。

- [ ] **Step 4: 提交**

```bash
git add cli/cache.go cli/cli_test.go
git commit -m "chore(cli): remove cache subcommand stubs now implemented"
```

---

## M1.D — E2E + 文档

### Task 18: E2E Q1 — 描述跨重启存活

**Files:**
- Modify: `test/e2e_test.go`

- [ ] **Step 1: 先看现有 e2e 测试如何构造 handler + mock vision + 跑请求**

Run: `go doc -all ./test 2>$null; ` 然后读 `test/e2e_test.go` 找既有 cache-hit 用例的 helper（如 `newMockVisionServer`、`buildHandler`）。

- [ ] **Step 2: 写失败测试 Q1**

参照既有 cache-hit e2e 用例，构造：mock vision 服务（返回固定描述）；首次请求断言 `vision_calls=1`（或等价 mock 命中计数 +1）；**关闭并重建 `cache.TwoTier`**（用同 db_path 重新 `OpenSQLite`+`NewTwoTier`，模拟重启）；同图再请求，断言 `cached=1`、mock 命中计数不增。

```go
func TestE2E_CacheSurvivesRestart(t *testing.T) {
	// 复用既有 newMockVisionServer / buildHandlerWithCache helper
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cache.db")

	// 首次：TwoTier 冷启动
	cold, _ := cache.OpenSQLite(dbPath, 10000, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tt1 := cache.NewTwoTier(10, cold, nil)
	h1 := buildHandlerWithCache(t, tt1) // 既有 helper，注入 cache

	// mock vision 只应被调用 1 次（首次）
	rr := sendImageRequest(t, h1)
	if code := rr.Code; code != 200 { t.Fatalf("status %d", code) }
	if got := mockVisionCallCount(t); got != 1 { t.Fatalf("vision calls want 1, got %d", got) }

	// "重启"：关旧 TwoTier，用同 db 重开
	cold.Close()
	cold2, _ := cache.OpenSQLite(dbPath, 10000, 0, nil)
	defer cold2.Close()
	tt2 := cache.NewTwoTier(10, cold2, nil)
	h2 := buildHandlerWithCache(t, tt2)

	rr2 := sendImageRequest(t, h2) // 同图
	if got := mockVisionCallCount(t); got != 1 { t.Fatalf("after restart vision calls want still 1, got %d", got) }
	// 断言响应头或 metrics 报 cached=1（按既有 e2e 约定）
	if !strings.Contains(rr2.Header().Get("X-Blind-Llm-Eyes"), "cached") {
		t.Errorf("response should report cached after restart: %s", rr2.Header().Get("X-Blind-Llm-Eyes"))
	}
}
```

helper 名（`buildHandlerWithCache`/`sendImageRequest`/`mockVisionCallCount`）需对齐既有 e2e_test.go 实际命名；若不存在则在该测试内 inline 实现最小版本（httptest upstream + mock vision + NewHandler）。

- [ ] **Step 3: 跑测试**

Run: `go test -race -count=1 ./test/ -run TestE2E_CacheSurvivesRestart`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add test/e2e_test.go
git commit -m "test(e2e): add cache survives restart scenario"
```

---

### Task 19: `config.example.yaml` 文档化

**Files:**
- Modify: `config.example.yaml`

- [ ] **Step 1: 读现有 `config.example.yaml`，在 cache 段补 type/db_path/sqlite_max_entries/sqlite_ttl 注释；在 vision_providers 段加 qwen 示例**

```yaml
cache:
  # type: lru            # 默认；内存 LRU，重启丢描述
  # type: twotier         # 两层 LRU+SQLite，描述跨重启存活
  # max_entries: 500      # LRU 热层容量
  # db_path: ./cache.db   # SQLite 路径（type=twotier）；空→./cache.db
  # sqlite_max_entries: 10000  # SQLite 容量上限；<=0→10000
  # sqlite_ttl: "720h"     # 30 天 TTL；空=不限
  max_entries: 500

# vision_providers（多 provider 池）示例：
# vision_providers:
#   - name: glm
#     type: glm_free          # GLM-4V-Flash 免费层，自动填 base_url/model
#     api_key: ${ZHIPU_API_KEY}
#   - name: qwen
#     type: qwen              # DashScope Qwen-VL，OpenAI 兼容，自动填 base_url/model
#     api_key: ${DASHSCOPE_API_KEY}
#     # model: qwen-vl-plus   # 可覆盖默认
```

- [ ] **Step 2: 跑全量门禁**

Run: `$env:CGO_ENABLED=0; go build ./... ; go test -race -count=1 ./... ; go vet ./...`
Expected: 全绿。

- [ ] **Step 3: 提交**

```bash
git add config.example.yaml
git commit -m "docs: document cache and qwen options in config example"
```

---

### Task 20: README / RELEASE_NOTES 提及新功能

**Files:**
- Modify: `README.md`（+ `README.zh-CN.md`），新建 `RELEASE_NOTES-v1.1.0.md`（+ zh）

- [ ] **Step 1: 在 README 的缓存段加持久化说明；在 provider 列表加 Qwen-VL**

- [ ] **Step 2: 写 `RELEASE_NOTES-v1.1.0.md` + zh 版**，要点：
  - 两层 LRU+SQLite 持久化缓存（opt-in：`cache.type: twotier`）
  - DashScope Qwen-VL 预设（`type: qwen`）
  - setup 向导 GLM/Qwen 预设修正（写 vision_providers+type）
  - 4 个 `cache` 子命令
  - 向后兼容（默认 LRU，零行为变化）

- [ ] **Step 3: 提交**

```bash
git add README.md README.zh-CN.md RELEASE_NOTES-v1.1.0.md RELEASE_NOTES-v1.1.0.zh-CN.md
git commit -m "docs: add v1.1.0 release notes and readme updates"
```

---

## M2 — RC1

### Task 21: tag RC1 + 本地构建验证

- [ ] **Step 1: 全量门禁**

Run: `$env:CGO_ENABLED=0; go build ./... ; go test -race -count=1 ./... ; go vet ./... ; goreleaser check`
Expected: 全绿。

- [ ] **Step 2: dry-run 构建 6 平台**

Run: `goreleaser build --snapshot --clean`
Expected: 6 二进制构建成功（linux/darwin/windows × amd64/arm64）。

- [ ] **Step 3: 手工 QA（DashScope 真实 key）**

用真实 DASHSCOPE_API_KEY 跑 `blind-llm-eyes doctor --deep`，确认 Qwen 视觉管道 PASS。

- [ ] **Step 4: tag + push（用户确认后）**

```bash
git tag v1.1.0-rc1
git push origin v1.1.0-rc1
```

---

## M3 — GA

### Task 22: tag v1.1.0 + 发布

- [ ] **Step 1: 最终全量门禁 + 手工冒烟（同 Task 21）**

- [ ] **Step 2: tag + push**

```bash
git tag v1.1.0
git push origin v1.1.0   # 触发 .github/workflows/release.yml
```

- [ ] **Step 3: 确认 GitHub Actions 6 平台发布成功**

- [ ] **Step 4: 发布 GitHub Release（用 RELEASE_NOTES-v1.1.0.md 内容）**

---

## Self-Review

**1. Spec coverage（对照 spec 各节）**
- §2.1 接口抽象 → Task 1 ✓
- §2.2 包结构 → Task 1/5/9 ✓
- §2.3 数据流（Get/Put/淘汰分治）→ Task 6/9 ✓
- §2.4 并发（TwoTier 互斥锁、SQLite WAL）→ Task 9 ✓
- §3.1 schema → Task 5 ✓
- §3.2 配置扩展 → Task 10 ✓
- §3.3 WAL/PRAGMA → Task 5 ✓
- §3.4 损坏恢复 → Task 8 ✓
- §3.5 main 装配 → Task 11 ✓
- §4.1 cache CLI 4 子命令 → Task 13-17 ✓
- §4.2 Qwen 预设 → Task 2 + 12 ✓
- §4.3 测试方案 → 各 Task 含 TDD + Task 18 e2e ✓
- §4.4 上线/向后兼容 → Task 11（降级）+ Task 19/20 文档 ✓
- spec 风险表"GLM 验证" → Task 12 设计上统一改写 vision_providers（比"先验证"更确定地正确）✓

**2. Placeholder scan**：无 TBD/TODO；所有代码步骤含完整代码。✓

**3. Type consistency**：`Cache` 接口、`*LRU`、`*SQLite`、`*TwoTier`、`BuildProvider` 的 `qwen` 派发、`runCache*` 签名在所有任务一致。`generateConfigYAML` 签名未变（仅函数体扩）。✓

**4. 注意事项**
- Task 9 的 `NewTwoTierForTest` 公开 helper 仅为避免循环，若 `twotier_test.go` 在 `cache` 包内则可直接用 `NewTwoTier`，删除该 helper。
- Task 18 helper 名需对齐既有 `test/e2e_test.go` 实际命名；若不匹配，在该测试内 inline 最小版本。
- PowerShell 环境：`$env:CGO_ENABLED=0` 语法（项目记忆约束）。
- 所有 commit 遵循 `.trae/rules/git-commit-message.md`（英文 conventional commit）。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-14-tier2-sqlite-cache-qwen.md`. Two execution options:

1. **Subagent-Driven (recommended)** — 每 Task 派新 subagent，Task 间评审，迭代快。
2. **Inline Execution** — 本会话用 executing-plans 批量执行，checkpoint 复审。

Which approach?
