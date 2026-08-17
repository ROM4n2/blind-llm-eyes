# Release Notes — v1.1.0

> ← Back to [README](README.md) · [v1.0.1 notes](RELEASE_NOTES-v1.0.1.md)

**blind-llm-eyes** — Give text-only LLMs eyes.

Release date: 2026-08-17
Scope: 21 commits since `v1.0.1` — Tier 2 (M0 + M1.A-D + perf patches)

---

## Overview

v1.1.0 adds two opt-in features on top of the v1.0.x foundation: a
**persistent two-tier cache** (LRU + SQLite) so image descriptions survive
proxy restarts, and a **DashScope Qwen-VL vision provider preset** for
users on the Alibaba Cloud ecosystem. Both are strictly opt-in — the
default behavior is unchanged from v1.0.1 (`cache.type: lru`, single
vision provider).

A new `cache` CLI subcommand exposes four operations (`path` / `stats` /
`list` / `clear`) for inspecting and managing the persistent store.

No breaking changes. Existing `config.yaml` files remain compatible; new
fields are optional with sensible defaults.

---

## New features

### 1. Two-tier persistent cache (LRU + SQLite)

[cache/sqlite.go](file:///d:/Code/new-api-contrib/cache/sqlite.go) ·
[cache/twotier.go](file:///d:/Code/new-api-contrib/cache/twotier.go) ·
[main.go](file:///d:/Code/new-api-contrib/main.go)

The default in-memory LRU cache loses all descriptions on restart. For a
personal proxy that processes dozens of screenshots per day, re-describing
every image after a restart wastes vision API quota and adds ~8s latency
per image.

The new `twotier` cache type adds a SQLite cold layer behind the LRU hot
layer:

- **Hot layer** — in-memory LRU, unchanged from v1.0.x. Sub-microsecond
  hits, no I/O.
- **Cold layer** — SQLite (pure-Go `modernc.org/sqlite`, no CGO). WAL
  journal mode for concurrent read/write. Descriptions survive process
  restarts, crashes, and redeploys.
- **Get flow** — hot miss → cold lookup under mutex → backfill hot →
  double-check prevents thundering herd.
- **Put flow** — dual-write to both layers (idempotent, lock-free).
- **Eviction** — LRU eviction clears only the memory copy; SQLite
  eviction (by `sqlite_max_entries` capacity or `sqlite_ttl` age) is the
  real delete.
- **Corruption self-heal** — `PRAGMA integrity_check` failure or
  "file is not a database" error triggers automatic deletion of the
  db/-wal/-shm files and a fresh empty database. Cold-start loses
  descriptions but never blocks the proxy.
- **Graceful degradation** — if SQLite open fails at startup, the proxy
  falls back to LRU-only with a WARN log, not a crash.

**Configuration:**

```yaml
cache:
  type: twotier              # opt-in; default lru (unchanged)
  max_entries: 500           # LRU hot-layer capacity
  db_path: ./cache.db        # SQLite path; default ./cache.db
  sqlite_max_entries: 10000  # cold-layer capacity cap
  sqlite_ttl: "720h"         # 30-day TTL; empty = unlimited
```

### 2. DashScope Qwen-VL vision provider preset

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go) ·
[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go)

A new `qwen` provider type auto-fills the DashScope (Alibaba Cloud
Bailian) OpenAI-compatible endpoint and model. Users only need an
`api_key` from `https://dashscope.aliyun.com`:

```yaml
vision_providers:
  - name: qwen
    type: qwen                    # auto-fills base_url + model
    priority: 1
    api_key: "sk-qwen-placeholder"
    # base_url: https://dashscope.aliyuncs.com/compatible-mode/v1  (auto)
    # model: qwen-vl-plus                                         (auto)
    # model: qwen-vl-max           # override to a stronger model
```

The `setup` wizard now offers Qwen-VL alongside GLM-4V-Flash and MiMo.
Both presets write `vision_providers[]` with the correct `type` field,
so the generated config works without manual editing.

### 3. `cache` CLI subcommand

[cli/cache.go](file:///d:/Code/new-api-contrib/cli/cache.go)

Four subcommands inspect and manage the persistent cache:

| Subcommand | Action |
|---|---|
| `cache path` | Print cache type, db path, and whether the db file exists |
| `cache stats` | Entry count, total bytes, oldest/newest access, db file size, journal mode |
| `cache list` | List entries (12-char hash prefix + 60-char description preview), `-limit N` |
| `cache clear` | Delete all entries, interactive `y/N` confirm or `-yes` to skip |

Each accepts `-config <path>` (default `config.yaml`). `stats` / `list` /
`clear` exit 1 if `cache.type` is `lru` (no persistent store). The CLI
opens SQLite with `busy_timeout=5000` so it works while the proxy is
running.

### 4. Performance: in-memory counter + CAS eviction guard

[cache/sqlite.go](file:///d:/Code/new-api-contrib/cache/sqlite.go)

Two perf patches land with the SQLite cache:

- **In-memory counter** — `atomic.Int64` tracks entry count, avoiding an
  `O(N) SELECT COUNT(*)` on every `Put`. The counter is initialized once
  at open and updated atomically; eviction checks read the counter, not
  the table.
- **CAS eviction guard** — `atomic.Bool` CAS ensures only one goroutine
  runs `evictIfNeeded` at a time, preventing concurrent eviction storms
  when many `Put` calls hit the capacity boundary simultaneously.

---

## Backward compatibility

- **Default cache type is `lru`** — existing configs with no `cache.type`
  field behave exactly as v1.0.1. Zero behavior change.
- **SQLite is opt-in** — the `cache.db` file is only created when
  `type: twotier` is explicitly set.
- **Qwen preset is additive** — the single-provider `vision:` field still
  works; `vision_providers` is only used when configured.
- **No new required config fields** — all new fields have defaults.

---

## Commits in this release

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

## Verification

- `go test -race -count=1 ./...` — 13 packages, all green
- `go vet ./...` — clean
- `go build ./...` — clean (CGO_ENABLED=0)
- E2E cross-restart test (`TestE2E_CacheSurvivesRestart`) — verifies
  descriptions survive a simulated proxy restart (close SQLite, reopen
  same db path, same image hits cold-layer cache, vision not called
  again)

---

## Upgrade notes

Drop-in replacement for v1.0.1. To enable the new features:

- **Persistent cache** — add to `config.yaml`:
  ```yaml
  cache:
    type: twotier
    db_path: ./cache.db           # optional, default ./cache.db
    sqlite_max_entries: 10000     # optional, default 10000
    sqlite_ttl: "720h"            # optional, 30-day TTL
  ```
  The `cache.db` file is created on first run. To inspect or clear it:
  `blind-llm-eyes cache stats` / `cache list` / `cache clear`.

- **Qwen-VL provider** — re-run `blind-llm-eyes setup` and pick the Qwen
  option, or add a `type: qwen` entry to `vision_providers` with just an
  `api_key` (base_url and model are auto-filled).

- **Cache management** — `blind-llm-eyes cache path|stats|list|clear`.
  See `blind-llm-eyes cache` for usage.
