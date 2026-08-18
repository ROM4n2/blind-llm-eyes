# v1.3.0 Design: Observability & Operations Enhancement

> **Date**: 2026-08-18
> **Version**: v1.3.0 (GA planned, no RC tag required — no breaking API changes)
> **Theme**: Observability & Operations Enhancement (可观测性与运维增强)
> **Approach Chosen**: Plan A — Balanced Ops (3×P0 + 5×P1, ≈12 commits, ≈6 weeks)
> **Predecessor Context**: v1.2.0 (2026-08-18) just shipped with 16 audit-risk fixes, security hardening, sharded cache locks, CI workflow, and optional /metrics auth. v1.3.0 builds on top with zero breaking changes.

---

## 0. Executive Summary

v1.3.0 closes the operational observability gaps that were intentionally left out of the security-focused v1.2.0 cycle. Users have reported: (a) changing log_level or swapping a vision provider requires a restart, causing a brief LRU cold-start cache miss storm; (b) Prometheus has no counters for cache tiered hits, circuit-breaker transitions, or SSE throughput — operators have to infer issues from raw logs; (c) there is no standard `go tool pprof` entry point when CPU / heap profiles are needed in production; (d) `blind-llm-eyes doctor` only validates config syntax and self-ref URLs but cannot answer "is the upstream actually reachable", "is SQLite writable", or "is my vision model name correct"; (e) the end-to-end test suite only exercises the `cache.NewLRU` code path so `TwoTier` bugs are only caught in production; (f) `config.example.yaml` is stale relative to v1.2.0 fields.

This spec answers each of (a)–(f) through 8 concrete work items organized as 4 task groups. Estimated ≈12 commits, ≈6 weeks, medium risk, high operational value.

---

## 1. Iteration Scope & Success Criteria

### 1.1 In-Scope (Plan A: Balanced Ops — 3 P0 + 5 P1)

| # | Priority | Item | Task Group |
|---|----------|------|-----------|
| 1 | **P0** | Config hot reload: `atomic.Pointer[Config]`, SIGHUP + admin HTTP `/admin/reload` + CLI `blind-llm-eyes reload` | TG1 / TG3 |
| 2 | **P0** | Metrics expansion P0 layer: cache counters + drift gauge, provider calls + CB transitions | TG2 |
| 3 | **P0** | `/debug/pprof/*` endpoint with 3-layer security (token / loopback / opt-in flag) | TG1 |
| 4 | P1 | E2E suite full TwoTier coverage (4× new tests) incl. cross-restart cache survival | TG3 |
| 5 | P1 | `config.example.yaml` sync: vision_capable_models block, metrics_auth_token block, vision.context_rounds *int semantics correction | TG1 |
| 6 | P1 | Doctor 3 new checks: DB writable / upstream reachable ping / vision model exists + /models warn | TG3 |
| 7 | P1 | Metrics expansion P1 layer: singleflight + images_bytes_in, req/resp payload size histograms, SSE events counter | TG2 |
| 8 | P1 | README + CHANGELOG update (reload triggers, pprof security, new metrics list, doctor new checks) | TG4 |

### 1.2 Explicitly Out of Scope (Deferred to later releases)

- Cache import/export (JSON dump / restore)
- Rate limiting (token bucket per-IP / per-token)
- Multi `-config` merge (overlay semantics)
- `/healthz` split into `/readyz` + `/livez` (k8s readiness probe)
- Admin web UI dashboard
- OpenTelemetry tracing (OTLP spans)
- Automatic cache drift repair (will remain WARN-only in v1.3.0; operator decides)
- Reload of `listen`, `upstream.base_url`, `upstream.api_key`, `metrics_auth_token`, `cache.db_path` (listed as Non-Reloadable in §2.2)

### 1.3 Success Criteria (Release Gates — all MUST pass)

- [ ] Items 1–8 (§1.1) implemented with unit tests
- [ ] `go test -race -count=1 ./...` — 100% PASS across 12 packages
- [ ] `go vet ./...` — clean
- [ ] `go build ./...` — clean
- [ ] `make snapshot` (goreleaser build --snapshot) — 6 platforms compile
- [ ] Pre-commit hooks (bash + PowerShell) — no hard-coded key patterns detected
- [ ] Backward compatibility: a v1.2.0-style `config.yaml` (without `metrics_auth_token` / `vision_capable_models`) loads without errors; deprecated legacy metric names `CacheHitRatio` and old `ProviderCallsTotal` still scrapeable from Prometheus (no counter reset panic)
- [ ] `TestE2E_CacheSurvivesRestart_TwoTier -race -count=10` — 0 flakes
- [ ] `go test -race -count=10 ./config ./metrics ./cli ./cache ./proxy` for specifically the TG2+TG3 hot paths — 0 failures

---

## 2. Feature 1: Config Hot Reload (P0)

### 2.1 Core Abstraction — `ReloadableConfig`

New package-local struct in `config/loader.go`:

```go
type ReloadableConfig struct {
    current atomic.Pointer[Config]
    mu      sync.Mutex       // serialize re-entrant reload (reload while reload)
    path    string           // config.yaml path, needed for re-reading
}

func NewReloadableConfig(cfg *Config, path string) *ReloadableConfig { ... }
func (r *ReloadableConfig) Load() *Config { return r.current.Load() }
func (r *ReloadableConfig) Reload() (prev, next *Config, err error) { ... }
```

**Concurrency contract**: `Load()` is a simple atomic read — zero blocking, can be called on every hot request path. `Reload()` takes a process-wide mutex to prevent overlapping reloads from SIGHUP and admin HTTP racing. If a reload is in-flight, a second `Reload()` caller blocks until the first completes (then returns the newly applied config, not a double-reload unless the file changed on disk in between — handled by comparing mtime).

**Old code pattern (to be replaced)**:
```go
// main.go today:
cfg, _ := config.Load(path)
handler := proxy.NewHandler(ctx, deps{Cache: cache, Vision: visionPool, Config: cfg})
// Problem: `cfg` held by value by all components; can never change mid-flight
```

**New pattern**:
```go
// main.go v1.3.0:
cfg, _ := config.Load(path)
rcfg := config.NewReloadableConfig(cfg, path)
handler := proxy.NewHandler(ctx, HandlerDeps{ReloadableConfig: rcfg, ...})
```

**Component adoption** (list of package reads):
- `proxy/handler.go`: Replace `d.cfg.*` reads with `d.rcfg.Load().*` reads for every request-scoped / tick-scoped field
- `vision/pool.go`: Accept `*config.ReloadableConfig`; add `Reconfigure(nextCfg *VisionCfg)` that builds a new Pool and atomically swaps after drain
- `cache/twotier.go`: `LRU.Resize(cfg.Cache.MaxEntries)` called on reload tick; TTL and sqlite_max read fresh on next eviction
- `logging/logging.go`: Logger built around `slog.LevelVar`; on reload call `levelVar.Set(parsedLevel)`

### 2.2 Reloadable vs Non-Reloadable Field Boundary

| Field | Reloadable | Strategy |
|-------|-----------|---------|
| `log_level` | ✅ Yes | `slog.LevelVar.Set()` — zero-downtime |
| `vision_providers[]` | ✅ Yes | Build new `vision.Pool`, drain old pool (30s timeout), atomically swap; old in-flight requests continue on old pool; new requests route to new pool |
| `vision.timeout` / `large_image_timeout` / `large_image_threshold` / `description_cap` / `supported_formats` / `context_rounds` / `context_max_chars` | ✅ Yes | Request-level `.Load()` reads — immediate |
| `adaptive_concurrency.*` | ✅ Yes | AIMD tick re-reads fresh values each tick |
| `cache.type` | ⚠️ Partial | lru↔twotier allowed (warm-start SQLite); **Cache pointer itself is never replaced on reload** to avoid races. Type change uses a lazy route: hot still hits LRU; on next cold miss we lazily build SQLite on first Put. Downside: lru→twotier means the 1st miss after reload still falls through (same cold-start semantics as restart). If `type` is unchanged then no-op. |
| `cache.max_entries` (hot) | ✅ Yes | `LRU.Resize(n)` in-place; downsizing evicts LRU entries |
| `cache.sqlite_max_entries` / `cache.sqlite_ttl` | ✅ Yes | Applied on next eviction tick |
| `fail_open` / `concurrency_limit` / `vision_capable_models` | ✅ Yes | Request-level reads |
| `metrics_auth_token` | ❌ No | Affects HTTP middleware chain assembly; changes require restart |
| `listen` | ❌ No | Process-level socket bind — cannot rebind |
| `upstream.base_url` / `upstream.api_key` | ❌ No | Would require hot-swapping the http.Transport, resolver, TLS session cache — too risky for in-flight requests |
| `cache.db_path` | ❌ No | SQLite connection pool cannot be safely closed-and-reopened while in-flight reads run |
| `max_body_bytes` | ⚠️ Partial | Reloads for *future* requests immediately; in-flight requests already streaming use the old limit (no back-pressure rollback) |

### 2.3 Reload Trigger Channels (dual channel, per user decision)

#### Channel 1: Unix SIGHUP

```go
// main.go:
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGHUP) // Windows: SIGHUP is not delivered by Go runtime; this channel simply never fires on Windows
go func() {
    for range sigCh {
        prev, next, err := rcfg.Reload()
        if err != nil {
            log.Warn("reload: kept old config", "err", err, "path", cfgPath)
            continue
        }
        applyReloadSideEffects(prev, next) // LevelVar + pool swap + LRU resize
        log.Info("reload: applied", "from_version", prev.VersionFingerprint(), "to_version", next.VersionFingerprint())
    }
}()
```

#### Channel 2: Admin HTTP `POST /admin/reload` + CLI `blind-llm-eyes reload`

The admin HTTP handler already exists (secured by per-process admin token from pidfile.json). Add a route:

```
POST /admin/reload  (requires existing admin token in X-Admin-Token header)
```

Response codes:
- `200 OK` — reload successful; body JSON `{"status":"ok","diff":["log_level:info→debug","adaptive_concurrency.enabled:false→true"]}`
- `422 Unprocessable Entity` — validation failed; body JSON `{"status":"kept_old","err":"yaml line 17: foo not a valid duration"}`
- `206 Partial Content` — reload succeeded but vision pool drain timed out (force-swap logged)
- `401 Unauthorized` — admin token missing or mismatched

CLI `blind-llm-eyes reload`:
```
Reads pidfile.json → reads admin token → POST http://<listen>/admin/reload → prints response.
Exit 0 on 2xx; exit 1 on 4xx/5xx (same exit codes as doctor).
Works cross-platform Linux/macOS/Windows — used as primary reload method on Windows where SIGHUP doesn't exist.
```

### 2.4 Rollback Protection

Inside `Reload()`:
1. Read bytes → `yaml.Unmarshal` into `*Config`
2. Run existing `validate(cfg)` function unchanged
3. IF any step fails → return `(oldCfg, oldCfg, err)`; no atomic.Swap, no side effects
4. IF passes → atomically swap pointer → THEN apply side effects (LevelVar, pool swap, LRU resize)
5. Side-effects order: `LevelVar` (idempotent) → `LRU.Resize` → `pool.Reconfigure + drain`

No step between swap and side-effects can panic, because we don't own the callers of Load() mid-stream. If pool drain panics it's a programming bug caught by -race test; recovery strategy in main is log+continue (not crash), same as any other v1.3.0 error path.

---

## 3. Feature 2: Metrics Expansion (P0 P1 layers)

### 3.1 Prometheus Registration Contract

All new metrics register to the **same** `prometheus.NewRegistry` already created by `NewMetrics()`. We do NOT create a second registry. Registering an existing name panics at startup — covered by the `-count=1` boot test.

### 3.2 New Metrics Table

#### P0 Layer (cache + provider + CB)

| Metric Name | Type | Labels | Notes / Instrumentation Sites |
|-------------|------|--------|-------------------------------|
| `blind_llm_eyes_cache_hits_total` | CounterVec | `tier` (hot/cold), `outcome` (hit/miss) | `cache/lru.go`: LRU.Get tier=hot; `cache/twotier.go`: first LRU hit tier=hot hit, LRU miss tier=hot miss, SQLite hit tier=cold hit, SQLite miss tier=cold miss |
| `blind_llm_eyes_cache_row_count` | Gauge | `tier` (memory/actual) | Writes at start; 60s tick update from `SQLite.ActualCount()` and `SQLite.MemoryCount()` |
| `blind_llm_eyes_cache_drift_pct` | Gauge | - | `(memory_count - actual_count) / max(actual_count, 1) * 100`. Saturate at ±100% to avoid inf/nan if actual_count temporarily returns 0 (empty DB). Write only when type=twotier. Writes on same 60s tick. |
| `blind_llm_eyes_provider_calls_total` | CounterVec | `provider`, `outcome` (success/fail/skip/fallback) | `vision/pool.go CallProvider`: success = err==nil; fail = provider returned err; skip = provider CB open pre-skip; fallback = next-provider after failure. Reuses existing `provider` label space; adds new `outcome` label (old queries still work — Prometheus aggregates by dimension). |
| `blind_llm_eyes_circuit_breaker_transitions_total` | CounterVec | `provider`, `from` (closed/open/halfopen), `to` (closed/open/halfopen) | `vision/circuit_breaker.go Transition()`: Each time state machine changes state, Inc with old and new state. Panic-guarded against invalid (from,to) combinations. Complementary to the existing `CircuitBreakerState` GaugeVec (which shows current state). |

#### P1 Layer (singleflight + images_bytes + payload + SSE)

| Metric Name | Type | Labels | Notes / Instrumentation Sites |
|-------------|------|--------|-------------------------------|
| `blind_llm_eyes_singleflight_total` | CounterVec | `phase` (exec/wait) | `proxy/handler.go`: Before `sf.Do(hash, fn)`, increment phase=exec for the caller that runs fn; after Do returns with shared=true, increment phase=wait for waiters. |
| `blind_llm_eyes_singleflight_merged_requests_total` | Counter | - | After `Do` returns, if the return value `shared==true` then `Inc()` by 1. Tracks how many waiters were saved from re-executing the vision call over time. |
| `blind_llm_eyes_images_bytes_in_total` | CounterVec | `format` | `messages/parse.go` where image base64 blocks are decoded: `base64.StdEncoding.DecodedLen(len(data))` → Add that value to the counter. |
| `blind_llm_eyes_request_body_bytes` | Histogram | - | Buckets = `[]float64{1e3, 1e4, 5e4, 1e5, 5e5, 1e6, 5e6, 2e7}` (spaced around 20MB max_body_bytes default). Recorded in a middleware wrapping the original body reader with `io.TeeReader` into a counting writer. |
| `blind_llm_eyes_response_body_bytes` | Histogram | - | Same buckets as request. Recorded after upstream response completes: for SSE streams — count written bytes as they flush; for non-SSE — read Content-Length or use actual bytes from a wrapped writer. |
| `blind_llm_eyes_sse_events_total` | Counter | `event` (message/error/other) | SSE passthrough parser in `proxy/handler.go`: scan `event:` lines before a blank line; count each occurrence. Default `event:` missing → label "message" per SSE spec. Non-SSE responses never touch this counter (0 always). |

### 3.3 Backward Compatibility for Deprecated Names

- Existing `CacheHitRatio` (Gauge): **Keep writing** (add comment `// Deprecated: use blind_llm_eyes_cache_hits_total / increase(5m) to compute hit ratio at any window`). DO NOT remove. Prometheus rules can migrate gradually over 1+ release.
- Existing `ProviderCallsTotal` (CounterVec `provider` label): Kept as-is (register unchanged). New `blind_llm_eyes_provider_calls_total` is a SEPARATELY NAMED counter with 2 labels. The rename avoids modifying the registered label-set which would require Prometheus scrape re-alignment.

### 3.4 Metric Label Cardinality Cap

Total cardinality budget:
- `provider_calls_total`: O(num_providers × 4 outcomes) = 8 typical; hard cap 40 (fail if config has >10 providers — already improbable)
- `cache_hits_total`: 2 tiers × 2 outcomes = 4 constant
- `cb_transitions_total`: O(num_providers × 9 pairs) — 9 transitions × 10 providers = 90 cap

Cardinality stays bounded by config limits. No unbounded labels (no "request_id" or per-image labels). Good Prometheus citizen.

---

## 4. Feature 3: debug/pprof Endpoint (P0)

### 4.1 Mount Path

Reuse the current `http.ServeMux` in main.go. Add a prefix handler for `/debug/pprof/`.

Standard Go `net/http/pprof` auto-registers on the default mux. To avoid that (we use a custom mux), use the low-level functions from `runtime/pprof` + handlers exposed by `net/http/pprof`:

```go
// In main.go route setup:
func mountPprof(mux *http.ServeMux, authToken string, pprofEnabled bool) {
    if !pprofEnabled {
        return
    }
    // 3-layer security middleware:
    secured := withPprofSecurity(pprof.Index, authToken)
    mux.HandleFunc("/debug/pprof/", secured)
    mux.HandleFunc("/debug/pprof/cmdline", withPprofSecurity(pprof.Cmdline, authToken))
    mux.HandleFunc("/debug/pprof/profile", withPprofSecurity(pprof.Profile, authToken))
    mux.HandleFunc("/debug/pprof/symbol", withPprofSecurity(pprof.Symbol, authToken))
    mux.HandleFunc("/debug/pprof/trace", withPprofSecurity(pprof.Trace, authToken))
}
```

Sub-pages heap/goroutine/allocs/threadcreate/block/mutex are handled by the main `pprof.Index` dispatcher under `/debug/pprof/`.

### 4.2 3-Layer Security (matches user decision: reuse metrics token + loopback fallback)

```
Layer 3 (if auth token != ""):
  ├─ token in query ?token=xxx or X-Metrics-Token header
  ├─ subtle.ConstantTimeCompare(provided, token)
  ├─ PASS → serve; FAIL → HTTP 401 "unauthorized" (same body as /metrics)

Layer 2 (if auth token == ""):
  ├─ ParseIP(req.RemoteAddr host part)
  ├─ IsLoopback() or req.Context() was already accepted by local loopback check
  ├─ PASS → serve; FAIL → HTTP 403 "forbidden: loopback or valid token required"

Layer 1 Guard:
  ├─ debug_pprof_enabled (new config flag, default true)
  └─ If false → DON'T REGISTER HANDLERS. Not even 404 — just not in mux tree.
```

Implementation function signature:

```go
func withPprofSecurity(next http.HandlerFunc, token string) http.HandlerFunc
```

Internally reuses helper `isLoopbackRemoteAddr(r)` (calls same normalizeHost helper `net.ParseIP.IsLoopback` as self-referential URL check — DRY, not duplicated).

**Rate limit on 401/403 responses**: Same 5-min dedup per unique-remote-addr — prevents log spam from scanners trying `/debug/pprof/heap`.

**No mutex/block profiling by default**: `runtime.SetMutexProfileFraction(0)` and `runtime.SetBlockProfileRate(0)` are left as Go defaults (off by default = 0 cost). Operator can enable via an env var `BLIND_PPROF_MUTEX_RATE=100` if needed — mentioned in README, no config key (infrequently used).

---

## 5. Feature 4: E2E TwoTier Full Coverage (P1)

### 5.1 Helper Extraction

In `test/e2e_test.go`, add:

```go
func setupHandlerBackend(t *testing.T, cacheImpl string) (h *proxy.Handler, cleanup func())
```

`cacheImpl = "lru"` returns the LRU-only backend we already use.
`cacheImpl = "twotier"` uses `t.TempDir()` + `cache.OpenSQLite()` + `cache.NewTwoTier()`. Both return a cleanup func.

### 5.2 New Test Cases

| Name | Backend | Goal |
|------|---------|------|
| `TestE2E_BasicRewrite_LRU` | LRU | Rename (currently unnamed) original test, keep passing |
| `TestE2E_BasicRewrite_TwoTier` | TwoTier | Same assertions; identical image payloads — validates that image rewriting works identically with SQLite path |
| `TestE2E_SSEPassthrough_LRU` | LRU | Existing, preserved |
| `TestE2E_SSEPassthrough_TwoTier` | TwoTier | Same assertions; SSE stream must not differ |
| `TestE2E_ModelUtilPassthrough_LRU` | LRU | Existing, preserved |
| `TestE2E_ModelUtilPassthrough_TwoTier` | TwoTier | vision_capable_models path not affected |
| `TestE2E_ShutdownPidfile` | LRU | Existing; orthogonal to cache backend (skipped for TwoTier) |
| **`TestE2E_CacheSurvivesRestart_TwoTier`** | TwoTier | **NEW CROSS-RESTART TEST** (see §5.3) |

### 5.3 Cross-Restart Survival Test (Most Valuable E2E)

```
Step 1: Create tmp SQLite file in t.TempDir/
Step 2: Proxy1 = build TwoTier against that db
Step 3: Send request with redPNG → verify vision client called (mock call count = 1)
Step 4: Call h.Shutdown(ctx) → which closes db via cache.Close (add Close method to cache.Cache interface if missing — needed for orderly shutdown path)
Step 5: Proxy2 = build TwoTier against SAME db file, fresh LRU, fresh mock vision client (reset counter)
Step 6: Send identical redPNG request → assert vision client call count still = 0 → assert response contains the cached description string
Step 7: Both proxies cleanup via t.Cleanup()
```

This test proves:
- SQLite file durable across process "restart" (two separate handler lifecycles in test process)
- Cold cache lookup works without the hot cache LRU state being preserved
- Cache description value is bit-identical to what was written before shutdown

### 5.4 cache.Cache.Close() Method (if absent)

Add `Close() error` to the Cache interface, implement in both LRU (no-op returning nil, just for interface) and TwoTier/SQLite: `s.db.Close()`. The handler will call `Close()` from its own shutdown path after the last request completes.

---

## 6. Feature 5: config.example.yaml Sync (P1)

### 6.1 Insertion Points

In `config.example.yaml`, after the `vision.*` block and before `cache:` block:
```yaml
# ── 原生视觉模型白名单（v1.2.0+ 默认空 = 始终重写） ──
# 若请求 model 命中以下名字（大小写不敏感，前后空白 trim），
# proxy 认为上游能原生看图，直接透传 body 不做图片替换。
# vision_capable_models:
#   - gpt-4o
#   - claude-3-5-sonnet-20240620
#   - claude-3-opus-20240229
```

After `log_level:` line:
```yaml
# ── /metrics & /debug/pprof 可选 token 认证（v1.2.0+） ──
# 留空 = 无需认证（Prometheus scraper 本地抓没问题）
# 若要远程访问 /metrics，建议设置一个 32B hex 的随机 token：
#   Linux/macOS: openssl rand -hex 32
#   Windows:     -join ((1..32 | ForEach-Object { '{0:x2}' -f (Get-Random -Max 256) }))
# 认证通过方式两种：?token=xxx 查询参数，或 HTTP 头 X-Metrics-Token。
# /debug/pprof/* 复用同一 token；当 token 为空时 pprof 仅允许回环源 IP。
# 也可通过环境变量 BLIND_METRICS_AUTH_TOKEN 设置，避免写入 YAML 明文。
# metrics_auth_token: ""
```

### 6.2 Comment Fix for vision.context_rounds

Current line:
```yaml
# context_rounds: 3   # 最近 N 轮对话传给 vision provider。0/不写 → 默认 3；写 -1 → 显式禁用
```
Replace with:
```yaml
# context_rounds: 3   # v1.2.0+ *int 语义：省略 key 或留空 → 默认 3 轮；显式写 0 或负数 → 禁用上下文感知（只传裸图）
```

This aligns with the `*int` pointer semantics (nil=default 3, *p=0→disabled).

---

## 7. Feature 6: Doctor 3 New Checks (P1)

### 7.1 New Checks Table

| # | Name | Implementation | Status Codes |
|---|------|---------------|-------------|
| D1 | `db_writable` | If cache.type != twotier → `SKIP`. Otherwise: (1) `OpenSQLite(db_path)` — FAIL if error; (2) `PRAGMA quick_check` — FAIL if returns anything other than "ok"; (3) `INSERT INTO doctor_write_probe(hash,description,size_bytes,created_at,last_accessed) VALUES(?,?,?,?,?)` with random hash then `DELETE` — FAIL if RowsAffected != 1. PASS only if (1)(2)(3) all green. | PASS / FAIL / SKIP |
| D2 | `upstream_reachable` | Reuse `cli/ping_upstream.go` ping logic: HTTP `HEAD` or `GET` to `upstream.base_url + /v1/models` (cheap) with 5s timeout. If ANY HTTP response received (even 401, 404, 405) = network is reachable. Only TCP connect timeout / TLS failure / DNS failure = FAIL. Log actual status code in output as informational. | PASS / FAIL |
| D3 | `vision_model_exists` | For each non-empty provider in vision_providers[] (or fallback to vision.base_url), use `vision/ping.go ProviderPing()` to check reachability. Then optionally call Anthropic-compatible `/v1/models` if the provider exposes it. If the returned list does NOT contain the configured model name (case-insensitive match), emit **WARN** with the mismatch info but don't FAIL (some providers don't implement `/models`). Only FAIL on hard errors: dial timeout, invalid auth (401 explicit), status 5xx from ping. | PASS / WARN / FAIL |

### 7.2 Output Format

Doctor output uses the same 4-column tabular format as today (name, status, duration, detail). New rows append AFTER the existing rows (`config_valid`, `self_referential_urls`) so user scripts that parse output are unaffected (new rows = additive).

Exit codes:
- 0 = zero FAILs (may contain WARNs, SKIPs, all PASS)
- 1 = one or more FAILs (regardless of PASSes)

Consistent with current doctor exit code semantics.

---

## 8. Feature 7: README + CHANGELOG Update (P1, TG4 last)

### 8.1 README Sections to Update / Append

**Security considerations (append)**:
- `/debug/pprof/*` auth semantics (Layer 3/2/1)
- Reload endpoints `/admin/reload` require admin token (pidfile.json)
- Hot reload cannot swap `listen`, `upstream.*`, `metrics_auth_token` — restart required list documented

**Configuration reference (append)**:
- `debug_pprof_enabled` row (insert near metrics_auth_token)
- Mention: `New in v1.3.0` tags on the 2 new rows + updated semantics row for vision.context_rounds

**Development (append)**:
- `make reload` shortcut command if added (optional; or documented via SIGHUP)
- Doctor output explanation of the 3 new checks and what FAIL/WARN means for each
- Prometheus scrape config suggestion example:
  ```yaml
  - job_name: blind-llm-eyes
    static_configs: [{targets: ['127.0.0.1:8790']}]
    params: {token: ['${BLIND_METRICS_AUTH_TOKEN}'}]}
    metrics_path: /metrics
  ```

### 8.2 CHANGELOG Structure

Follow the exact v1.2.0 format (4 sections: Security / Reliability / Compatibility / Engineering). v1.3.0 contributions map to:

- **Reliability**: config hot reload + E2E coverage + doctor checks
- **Observability (new top-level section)**: metrics P0/P1 + pprof endpoint
- **Compatibility**: ReloadableConfig non-breaking changes (interface additions only, no removals)
- **Engineering**: example.yaml sync + docs update

### 8.3 No New RELEASE_NOTES-*.md File Required

We already have CHANGELOG.md as the canonical changelog source. RELEASE_NOTES-*.md is historically reserved for major/first releases (v1.0.0, v1.0.1, v1.1.0). v1.3.0 can reuse the CHANGELOG block directly in the GitHub Release body, consistent with v1.2.0.

---

## 9. Error Handling Matrix (Unified)

| Scenario | Behavior | Log Level | HTTP Exit Code if applicable |
|----------|----------|----------|------------------------------|
| YAML parse fail during reload | Keep old config; return err from Reload() | WARN | POST /admin/reload → 422 |
| Field validation fail during reload | Keep old config | WARN | 422 with detail |
| Pool drain timeout (>30s) | Force swap; let old in-flight finish via ctx | WARN | POST /admin/reload → 206 "partial" |
| Reloadable field changed but adapter says No | Ignore silently; log what was skipped during apply | INFO | 200 (partial-skip info in body diff JSON) |
| cache_row_drift_pct ABS > 10% | Metrics gauge reflect; cache stats warn; slog warn | WARN | N/A |
| pprof called externally with token=="" + non-loopback | 403 body; per-remote-addr 5-minute dedup log | INFO (dedup) | 403 |
| pprof called with mismatched token | 401; same dedup log | WARN | 401 |
| Doctor D1 SQLite can't write | FAIL detail + exit 1 | INFO (doctor stdout only) | N/A |
| Doctor D2 upstream can't dial | FAIL detail + exit 1 | INFO | N/A |
| Doctor D3 vision ping 401 | FAIL detail + exit 1 | INFO | N/A |
| Doctor D3 /models 404 (provider doesn't implement models) | WARN, exit 0 | INFO | N/A |

---

## 10. Test Strategy & Quality Plan

### 10.1 TDD Discipline Required

For every work item:
1. Write failing test first (Red)
2. Implement minimal code (Green)
3. Refactor in-place (Refactor)
4. Commit using project convention: `type(scope): lowercase action-first summary`

### 10.2 Unit Test Count Target

| Package | New tests | Approximate coverage delta |
|---------|-----------|---------------------------|
| `config/` | 8-10 | ReloadableConfig: atomic swap + concurrent load + field boundary + reload rollback |
| `metrics/` | 6-8 | New counters incr; labels correct; drift gauge saturates; register no-dup |
| `cache/` | 4-6 | LRU.Resize; TwoTier lazy build on cache.type reload; Close() method |
| `vision/` | 4-6 | pool.Reconfigure drain/no-drain; pool with in-flight + transition |
| `cli/` | 4-6 | `reload` CLI exit codes; doctor D1/D2/D3 in each PASS/FAIL/WARN/SKIP branch |
| `proxy/` | 6-8 | pprof 3-layer middleware; singleflight counters; request/response sizes; SSE event counting |
| `main` (intg via cli_test) | 2-3 | SIGHUP handling: emit fake signal via kill(getpid, SIGHUP) on Unix only; Windows skip |
| **Total** | **≈34-40 new unit tests** | |

### 10.3 E2E Test Count Target

- 4 new TwoTier tests (BasicRewrite / SSEPassthrough / ModelUtilPassthrough / CacheSurvivesRestart)
- Total E2E tests grow from 5 (today) to 9.

### 10.4 Race Stress

Target 1 additional concurrency stress test:
```
TestReloadConfig_ConcurrentRequests
  → 10 reload goroutines + 100 handler request goroutines × 3s
  → Assert: no race detected by -race, no panics, reloads succeed
```

### 10.5 Flakiness Prevention Gate

`TestE2E_CacheSurvivesRestart_TwoTier -race -count=10` must be 10/10 before final merge. Run during CI: add a step to ci.yml (ubuntu-only job, `-count=10` race only for `./test/` package to keep matrix time acceptable — ubuntu 5min budget).

---

## 11. Task Groups, Dependencies, Milestones

### 11.1 Task Group Ordering (TG1 → TG4)

```
TG1 Infrastructure [week 1-2, no cross dependencies]
├─ [P0] ReloadableConfig abstract + atomic.Swap contract + validation-only unit tests
├─ [P0] debug/pprof handler mount + 3-layer security middleware + unit tests
└─ [P1] config.example.yaml sync + context_rounds comment fix
Exit: interfaces settled, reloadable abstraction usable downstream

TG2 Observability Metrics [week 2-3, depends on TG1 ReloadableConfig for periodic tick reads]
├─ [P0] Metrics P0 layer: cache_hits_total / cache_row_count / cache_drift_pct
├─ [P0] Metrics P0 layer: provider_calls_total / cb_transitions_total
├─ [P1] Metrics P1 layer: sf_total / sf_merged / images_bytes_in
└─ [P1] Metrics P1 layer: req_body_bytes_hist / resp_body_bytes_hist / sse_events_total
Exit: Every new counter has at least 1 unit test confirming incr()

TG3 Integrations [week 3-5, depends on TG1 interfaces + TG2 counter plumbing where needed]
├─ [P1] E2E suite TwoTier coverage + CacheSurvivesRestart
├─ [P1] Doctor 3 new checks (each with unit test PASS/FAIL branches)
└─ [P0] Hot reload triggers: SIGHUP goroutine + /admin/reload HTTP + CLI `reload` subcommand
Exit: All observable features work end-to-end

TG4 Docs + Release Gate [week 5-6, last, depends on all previous]
├─ [P1] README update sections: security config reference dev docs
└─ [P1] CHANGELOG section write; perform Release Gate 12-point check; tag v1.3.0
Exit: v1.3.0 GA published
```

### 11.2 Milestones

- **M1 — Feature Complete (end of week 4)**: TG1 + TG2 + TG3 tasks done. Developer can run `make test` and use hot reload + pprof + metrics locally. Unstable, not released.
- **M2 — Release Ready (end of week 6)**: TG4 tasks done, Release Gate §1.3 all items checked, `make snapshot` 6 platforms build.

### 11.3 Estimated Commit Breakdown

- TG1: 4 commits (Reloadable abstract / pprof / yaml sync / tests)
- TG2: 3 commits (P0 metrics cache / P0 metrics provider / P1 metrics sf+payload+SSE)
- TG3: 3 commits (E2E TwoTier / Doctor 3 checks / reload channels)
- TG4: 2 commits (README + CHANGELOG; final gate + version stamps)
- **Total: ≈12 commits** (matches Plan A estimate)

---

## 12. Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|-----------|
| Pool drain during reload deadlocks because in-flight requests hang | Medium | 30s timeout enforced by `time.After` in drain wait loop + panic on programming bugs caught by -race |
| `atomic.Pointer[Config]` swap causes a request that was HALFWAY through reading cfg fields to observe inconsistent state (part old, part new) | High | **All reads per request must do ONE `Load()` call into a local var, then read fields off that frozen snapshot** — enforced by code review pattern. If a request path reloads the config pointer twice, that's a bug. Unit test: spin 100 goroutines each reading multiple fields and assert they belong to same version fingerprint (a hash written into Config for tests). |
| pprof endpoint leak of heap profile to remote attacker | High | 3-layer security. Also, Layer 2 (loopback check) verified via unit tests. CI has a test with `httptest.NewServer` that fabricates `RemoteAddr="1.2.3.4:12345"` and asserts 403. |
| Doctor upstream_reachable HEAD gives false PASS if upstream returns 500 (indicating failure, not reachability) | Low | Only TCP errors fail; 5xx is a "server is reachable and responding" result. It's valuable to show PASS because network layer works; the 5xx is already visible in upstream request metrics 5xx counter. |
| Cross-restart E2E test flaky due to temp dir cleanup timing | Low | Use `t.TempDir()` for per-test-isolated dirs; handler.Close() explicitly before next step; use `-count=10` pre-release gate to detect flakiness early. |
| Reloadable vs Non-Reloadable boundary gets forgotten; code adds a new field later and expects it to reload without updating this spec | Low | Release gate checklist includes "§2.2 boundary still matches code". Code comments above each field type in config.Config struct have `// Reloadable: yes/no/partial` annotations. |

---

## 13. Self-Review Checklist (Pre-implementation)

- [x] No TODO placeholders / TBD values — all sections concrete
- [x] No contradictions: TG3 depends on TG1 interfaces; no circular dependency
- [x] Scope matches Plan A (3 P0 + 5 P1): exactly 8 items, no scope creep
- [x] All ambiguous choices resolved explicitly:
  - pprof default enabled (conservative user can turn off via `debug_pprof_enabled`)
  - `cache.type` partial reloadable, not fully swapable
  - `Reload()` keeps old config on ANY validation fail (never partial apply)
  - Metrics keep deprecated name for 1+ release window
- [x] Every feature entry has an explicit failure mode + test plan
- [x] Semantic versioning respected: NO breaking changes. Interface additions only (ReloadableConfig type, cache.Cache.Close method). No method removals or signature changes on existing exported symbols.
- [x] Release gates are quantified, not vague
- [x] Risk section addresses the 6 top failure modes I could identify

---

## 14. Approval

Reviewed & approved by: ______________________________________ Date: _________

After approval: proceed to `writing-plans` skill which generates the task-by-task implementation plan matching these specs.
