# blind-llm-eyes

**English** | [中文](README.zh-CN.md)

[![ci](https://github.com/ROM4n2/blind-llm-eyes/actions/workflows/ci.yml/badge.svg)](https://github.com/ROM4n2/blind-llm-eyes/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/ROM4n2/blind-llm-eyes?sort=semver)](https://github.com/ROM4n2/blind-llm-eyes/releases)
[![license](https://img.shields.io/github/license/ROM4n2/blind-llm-eyes)](LICENSE)

Give text-only LLMs eyes. A reverse proxy that sits in front of an Anthropic-compatible, text-only model and transparently turns image blocks into text descriptions using a separate vision model — so you can paste screenshots into Claude Code without switching providers.

**Primary scenario:** Claude Code ↔ **blind-llm-eyes** ↔ text-only upstream (e.g. DeepSeek), with a vision model (e.g. MiMo) describing the images.

```text
Claude Code  ──►  blind-llm-eyes  ──►  text-only model (DeepSeek / Anthropic-compatible)
                     │
                     └──► vision model (MiMo)  ──► image descriptions
```

## Why

DeepSeek has no vision. Using it from Claude Code means pasting a screenshot gets a "I can't see images" reply, and the fix — switching to a vision-capable provider mid-conversation — breaks the workflow.

`blind-llm-eyes` removes the friction: images stay in the conversation, get described once, and the text-only model reads the descriptions as if it saw the image.

## Features

- **Anthropic Messages passthrough** — accepts `/v1/messages`, rewrites the request, forwards to any Anthropic-compatible upstream, and streams the SSE response back byte-for-byte.
- **Image → description replacement** — image blocks are replaced in place with `<BLIND_LLM_EYES_IMAGE>`-wrapped text, and a system instruction tells the model to treat the description as its own observation.
- **Nested `tool_result` images** — recursively finds images nested inside `tool_result` blocks (where real Claude Code screenshots usually live) and describes them like top-level ones (recursion depth capped at 16 to prevent stack overflow).
- **Conversation-context-aware descriptions** *(optional)* — feeds the last N turns of conversation (`context_rounds`, default 3 rounds / `context_max_chars` 2000 chars) to the vision model so descriptions match intent (e.g. "how do I fix this error?" focuses on the error text).
- **Content-hash cache with optional persistence** — the same image re-sent across turns triggers zero vision calls. Default is in-memory LRU; opt-in two-tier (LRU + SQLite) keeps descriptions across restarts (`cache.type: twotier`).
- **`singleflight` in-flight dedup** — concurrent requests carrying the same image share a single vision call.
- **Parallel image processing** — images in one request are described concurrently via `errgroup`, bounded by `concurrency_limit`.
- **Adaptive concurrency** *(optional)* — AIMD-style controller that raises/lowers the concurrency limit from real vision latency feedback (P90 + error rate), protecting against slow upstreams.
- **fail-open** — a failed vision call replaces the image with a placeholder instead of blocking the whole request.
- **WebP → PNG conversion** — automatically converts WebP images before sending them to the vision model.
- **Adaptive timeouts** — large images get a longer timeout (`large_image_timeout`).
- **Observability** — structured JSON logs (async writer), per-stage timing via `httptrace`, Prometheus metrics at `/metrics`, request IDs threaded through the whole pipeline, graceful shutdown.
- **Pluggable vision backends** — anything implementing `vision.VisionProvider` works. Built-in presets: MiMo (Anthropic format), OpenAI-compatible, GLM-4V-Flash (free tier), Qwen-VL (DashScope).
- **Multi-provider pool with circuit breakers** — `vision_providers` defines a priority-ordered list; failed providers trip a circuit breaker and traffic fails over automatically.
- **Single static binary** — no runtime dependencies, ~10 MB.
- **Model name sanitization** — automatically strips vendor context-length suffixes before forwarding to upstream (`deepseek-chat[1m]` → `deepseek-chat`); applied both on the request path and during cc-switch import (double-safe).
- **CLI lifecycle** — 9 subcommands cover the full lifecycle: `setup` (interactive config + doctor), `doctor` (connectivity self-check), `connect`/`disconnect` (Claude Code settings.json wiring), `start`, `status`, `stop`, `version`, `cache` (inspect/clear the persistent cache).
- **cc-switch one-click import** — reads providers directly from the cc-switch SQLite database (best-effort: falls back to temp-copy on DB lock, falls back to manual input on any error).
- **Safe settings management** — `connect` rewrites Claude Code's `settings.json` with a full-file backup taken exactly once (repeat `connect` never overwrites the backup); `disconnect` restores byte-for-byte from the backup via atomic writes.

## Performance results

Measured on production traffic against the MiMo vision model (2026-08-12 smoke test, 20 samples). Every number is a real before/after measurement, not an estimate.

| Optimization | Before | After | Gain |
| --- | --- | --- | --- |
| Disable MiMo thinking mode | 23,500 ms body read | 4,153 ms | **-82%** |
| Parallel image processing (`errgroup`) | 39,689 ms (2-image E2E) | 19,754 ms | **-50%** |
| Dedup in-flight vision calls (`singleflight`) | 5 identical images → 5 calls | → 1 call | **N→1** |
| AIMD adaptive concurrency | static `limit=4` | dynamic `[1,12]`, self-tuned | up to +5 / down to 1 |

Details:

- **Disable thinking mode** — the largest single win. `body_read` dominated the vision call; root cause was MiMo's default thinking mode emitting hidden reasoning output (2,257 chars). Switching to the Anthropic Messages API with `thinking.type: disabled` cut body read from 23.5 s to 4.2 s (**-82%**), total vision call from 31.7 s to 12.4 s (**-61%**), and reasoning output to 0.
- **Parallel image processing** — serial → `errgroup` with a bounded concurrency: 2-image end-to-end 39.7 s → 19.8 s (**-50%**).
- **In-flight dedup** — `singleflight` + content-hash LRU: 5 identical images in one request collapse to 1 vision call; 10 concurrent requests carrying the same image also collapse to 1 (**N→1**).
- **AIMD adaptive concurrency** — tuned from 20 production samples (MiMo avg 7.7 s, worst 20.6 s): defaults set to `concurrency_limit: 6`, `max_limit: 12`, `sample_window: 10`, `cooldown_ms: 2000`. Three-phase verification: fast upstream (P90≈3 s) → limit auto-rises to 9; normal (P90≈11 s) → hysteresis holds at 10; slow (P90≈16 s) → limit drops to 1.

## Quick start

Three subcommands do the heavy lifting: `setup` (interactive config), `connect` (wire Claude Code to the proxy), and `start` (run the proxy). The whole flow is download → `setup` → `connect` → `start`.

### 1. Install

Download a precompiled binary from the [releases page](../../releases) (Windows / Linux / macOS, amd64 + arm64), or build from source:

```bash
go install github.com/ROM4n2/blind-llm-eyes@latest
# or, from a checkout:
go build -o blind-llm-eyes .
```

Verify the install:

```bash
blind-llm-eyes version   # blind-llm-eyes <version> (go <runtime>)
```

### 2. Configure (`setup`)

Run the interactive wizard. It can import providers from your existing [cc-switch](https://github.com/farion1231/cc-switch) database, then runs a connectivity self-check (`doctor`) before saving:

```bash
blind-llm-eyes setup
```

The wizard collects an upstream (text-only) and a vision provider — base URL, API key, and vision model — pings both, and writes `config.yaml`. Prefer manual editing? Copy `config.example.yaml` to `config.yaml` and fill in real keys. Minimal working config:

```yaml
listen: "127.0.0.1:8790"
upstream:
  base_url: "https://api.deepseek.com/anthropic"   # text-only upstream (Anthropic-compatible)
  api_key: "sk-..."                                # optional: overrides client Authorization
vision:
  base_url: "https://api.xiaomimimo.com/anthropic" # vision model root; the client appends /v1/messages
  api_key: "sk-..."
  model: "mimo-v2.5"
fail_open: true
log_level: "info"
```

`config.yaml` is git-ignored; `config.example.yaml` is committed with placeholders. Secrets can also be provided via env vars (`BLIND_VISION_API_KEY`, `BLIND_UPSTREAM_BASE_URL`, `BLIND_UPSTREAM_API_KEY`, `BLIND_LISTEN`).

### 3. Connect Claude Code (`connect`)

Point Claude Code at the proxy by rewriting `~/.claude/settings.json`'s `env.ANTHROPIC_BASE_URL`:

```bash
blind-llm-eyes connect
```

A full backup is written to `~/.claude/.bak-before-connect` (never overwritten on repeat `connect`). Restart Claude Code so it re-reads `settings.json`. Undo with `blind-llm-eyes disconnect` — it restores `settings.json` byte-for-byte from the backup.

### 4. Run (`start`)

```bash
blind-llm-eyes            # no args = start (backward compat)
blind-llm-eyes start      # explicit
blind-llm-eyes -config /path/to/config.yaml
```

Manage the running proxy:

```bash
blind-llm-eyes status     # pidfile + GET /healthz → RUNNING / STALE
blind-llm-eyes stop       # POST /admin/shutdown (token-authed) → graceful drain
blind-llm-eyes doctor     # full connectivity self-check (upstream + each vision provider)
```

### 5. Verify

Paste a screenshot into Claude Code — the text-only model should now answer questions about it. Or curl directly:

```bash
curl -N http://127.0.0.1:8790/v1/messages \
  -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","max_tokens":500,"stream":true,"messages":[{"role":"user","content":[
    {"type":"text","text":"What is in this image?"},
    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<base64>"}}]}]}'
```

Response header `X-Blind-Llm-Eyes` reports the outcome: `rewritten=1 cached=0`.

> **Note on CC Switch:** set the provider's `ANTHROPIC_BASE_URL` to `http://127.0.0.1:8790`. Do **not** use CC Switch's proxy mode — it truncates image bodies.

### Verification & troubleshooting

Use the 5-step progressive verification to isolate issues without guessing:

```powershell
# L1 — Binary & version injection
blind-llm-eyes version
# → blind-llm-eyes 1.0.0 (go go1.26.5)

# L2 — Connectivity (zero or ~1 token consumed)
blind-llm-eyes doctor
# → upstream=PASS  vision=PASS   (exit 0)
# If any FAIL: check base_url (no trailing slash, correct /anthropic vs /v1),
#              check API key via env var or config.yaml

# L3 — Process liveness
# terminal A: blind-llm-eyes start
# terminal B:
blind-llm-eyes status
curl -s http://127.0.0.1:8790/healthz
# → status: RUNNING pid=1234 addr=127.0.0.1:8790
# → healthz: ok

# L4 — End-to-end (small API quota consumed)
curl -N http://127.0.0.1:8790/v1/messages `
  -H "Authorization: Bearer <upstream-key>" -H "Content-Type: application/json" `
  -d '{\"model\":\"deepseek-chat\",\"max_tokens\":500,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"What color is this?\"},{\"type\":\"image\",\"source\":{\"type\":\"base64\",\"media_type\":\"image/png\",\"data\":\"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg==\"}}]}]}' `
  -D - 2>&1 | Select-Object -First 50
# Look for: HTTP/1.1 200 OK  +  X-Blind-Llm-Eyes: rewritten=1 cached=0
#           then the SSE stream containing the vision description text

# L5 — Graceful shutdown
blind-llm-eyes stop
blind-llm-eyes status
# → NOT RUNNING
```

Common gotchas:

| Symptom | Cause | Fix |
| --- | --- | --- |
| `doctor` reports vision `PASS` but L4 returns 502 with `"vision call failed"` | Real `DescribeImage` (larger payload + longer timeout) fails where `Ping` (1-token) succeeds. Common when vision timeout is too small. | Raise `vision.timeout` (default 30s) and confirm the vision model accepts images at its configured endpoint. |
| `status` returns `NOT RUNNING` even though `start` is running in foreground | On Windows Trae IDE terminals, `os.CreateTemp` for the pidfile is blocked by the sandbox (sandbox error: `Not allow operate files: ...pidfile-*.tmp`). This only affects the IDE-integrated terminal. | Run `status` / `stop` from a standalone PowerShell window. The foreground `start` itself works everywhere, including inside Trae. |
| Upstream returns 400 `"model: deepseek-chat[1m] not found"` | `[1m]` suffix reached upstream (model sanitization is not active). Older pre-v1.0.0 builds didn't strip the suffix; or a reverse proxy in front of blind-llm-eyes is re-injecting the original model. | Upgrade to v1.0.0+ (`blind-llm-eyes version` confirms). Confirm the `ANTHROPIC_MODEL` env in Claude Code / cc-switch doesn't override the model field sent through the proxy — the sanitization only runs *inside* the proxy on the parsed request body. |
| Claude Code still says "I can't see images" after `connect` | Claude Code reads `settings.json` on startup only; or a cc-switch switcher run after `connect` overwrote `ANTHROPIC_BASE_URL` back. | Restart Claude Code. Run `connect` again and **don't** switch providers in cc-switch while using the proxy. |
| Images get truncated / vision hash mismatch errors | Using CC Switch **proxy mode** (not base_url override). It silently truncates request bodies > ~200 bytes before forwarding. | Set `ANTHROPIC_BASE_URL=http://127.0.0.1:8790` directly. Don't enable proxy mode in cc-switch. `blind-llm-eyes connect` writes exactly this setting. |
| `go install github.com/ROM4n2/blind-llm-eyes@latest` errors with "invalid version: unknown revision" | `@latest` requires at least one published semver tag on the remote repo. Tag `v1.0.0` exists locally but hasn't been pushed yet. | Download a prebuilt archive from the releases page, or build from a checkout: `go build -ldflags "-X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version=1.0.0" .` |

### Cache management

When `cache.type: twotier` is enabled, the SQLite cold layer persists across restarts. Four `cache` subcommands inspect and manage it:

```powershell
blind-llm-eyes cache path                  # show cache type, db path, file existence
blind-llm-eyes cache stats                 # entries, total bytes, oldest/newest access, db size
blind-llm-eyes cache list -limit 50        # list entries (hash prefix + description preview)
blind-llm-eyes cache clear -yes            # delete all entries (-yes skips confirmation)
```

Each subcommand accepts `-config <path>` (default `config.yaml`). `stats` / `list` / `clear` exit 1 if `cache.type` is `lru` (no persistent store). The CLI opens the SQLite db with `busy_timeout=5000` so it can inspect/clear the cache even while the proxy is running.

### Security considerations

- **Admin token** — generated fresh from `crypto/rand` on every `start`, written to the pidfile at `<UserConfigDir>/blind-llm-eyes/pidfile.json`, and deleted by `stop`. It's never persisted elsewhere (no env var, no config key).
- **Bind address** — default listen is `127.0.0.1:8790` (loopback only). Exposing `0.0.0.0` or a LAN IP forwards `/v1/messages` with your upstream API key to anyone who can reach the port — never do that on an untrusted network.
- **`/metrics` authentication (optional)** — by default `/metrics` is unauthenticated (for Prometheus scrapers on localhost). Set `metrics_auth_token` in `config.yaml` or the `BLIND_METRICS_AUTH_TOKEN` env var to require a token via `?token=xxx` query param or `X-Metrics-Token` HTTP header. The comparison uses constant-time (`crypto/subtle.ConstantTimeCompare`) so timing attacks cannot extract the token byte-by-byte.
- **Keys from config vs env** — API keys set in `config.yaml` override the client's `Authorization` header. The handler strips `Authorization` / `Proxy-Authorization` / `Cookie` headers from the client **whenever `UpstreamAPIKey` is configured** (proxy injects its own key). When `UpstreamAPIKey` is empty, the proxy acts as a transparent forwarder and passes the client's `Authorization` to the upstream so it can authenticate directly. Client credentials are never logged; context text in logs is truncated to 80 bytes as `context_preview`.
- **Pidfile permissions** — on Windows the pidfile directory defaults to `%AppData%\blind-llm-eyes\` (user-only ACLs inherited from `AppData`). Don't share `%AppData%` across accounts or put it on a world-readable share.
- **`connect` backup** — `~/.claude/.bak-before-connect` is a verbatim copy of your original `settings.json`. It contains whatever secrets `settings.json` held (e.g. `ANTHROPIC_API_KEY`). Treat it the same as the real file.
- **Pre-commit hooks** — the repo ships with `pre-commit` (bash) and `pre-commit.ps1` (PowerShell) hooks that block hard-coded API keys (`sk-*`, `AKIA*`, `ghp_*`) from reaching git history. Run `git config core.hooksPath .githooks` once after cloning to activate.

## Configuration reference

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `127.0.0.1:8790` | Bind address |
| `upstream.base_url` | — (required) | Text-only upstream root (Anthropic-compatible) |
| `upstream.api_key` | — | If set, overrides the client's `Authorization` when forwarding; client auth headers are stripped only when this is configured |
| `vision.base_url` | — (required) | Vision model root; the client appends `/v1/messages` |
| `vision.api_key` | — | Vision provider key |
| `vision.model` | — | Vision model name |
| `vision.timeout` | `30s` | Default vision call timeout |
| `vision.large_image_timeout` | `120s` | Timeout for images ≥ `large_image_threshold` |
| `vision.large_image_threshold` | `1048576` | Bytes; images at/above this get the large timeout |
| `vision.description_cap` | `1000` | `max_tokens` for descriptions |
| `vision.supported_formats` | png/jpeg/webp/gif | Allowed media types |
| `vision.context_rounds` | (omitted = `3`) | Context-aware descriptions: last N turns. Omit the key entirely for the default 3. Set explicitly to `0` to disable context awareness. Negative values are clamped to 0. |
| `vision.context_max_chars` | `2000` | Max context chars (~500 tokens) |
| `vision_capable_models` | `[]` (always rewrite) | Model names that natively support image input (e.g. `gpt-4o`, `claude-3-5-sonnet`). When the request model matches (case-insensitive, after sanitization), the proxy skips image rewriting and forwards the body verbatim. |
| `cache.type` | `lru` | `lru` (in-memory) or `twotier` (LRU + SQLite, descriptions survive restart) |
| `cache.max_entries` | `500` | LRU hot-layer capacity (total capacity when `type=lru`) |
| `cache.db_path` | `./cache.db` | SQLite cold-layer path (only when `type=twotier`) |
| `cache.sqlite_max_entries` | `10000` | SQLite cold-layer capacity cap |
| `cache.sqlite_ttl` | `0` (unlimited) | Cold-layer entry TTL, e.g. `720h` for 30 days |
| `concurrency_limit` | `4` | Max parallel vision calls per request; also the adaptive initial value |
| `adaptive_concurrency.*` | disabled | AIMD controller (see below) |
| `fail_open` | `false` | Vision failure → placeholder instead of 502 |
| `log_level` | `info` | `debug`/`info`/`warn`/`error` |
| `metrics_auth_token` | — (no auth) | Optional bearer token for `/metrics`. Set via env var `BLIND_METRICS_AUTH_TOKEN` to avoid storing secrets in YAML. |
| `max_body_bytes` | `20971520` (20 MB) | Max request body size; larger requests are rejected with 413 |

### Adaptive concurrency

Mirrors TCP congestion control. Every real vision call (the `singleflight` executor only, so each sample reflects actual upstream latency) goes into a rolling window; when the window fills, the P90 latency and error rate decide the new limit:

- P90 < `fast_threshold_ms` and no errors → `+increase_step` (additive increase)
- P90 > `slow_threshold_ms` or error rate > `error_threshold` → `×decrease_ratio` (multiplicative decrease)
- otherwise → unchanged (hysteresis band prevents oscillation)

Disabled by default; when disabled, behavior is identical to a static `concurrency_limit`. Production smoke-test tuning (2026-08-12): MiMo averages ~7.7 s, worst 20.6 s, so the defaults are `concurrency_limit: 6`, `max_limit: 12`, `sample_window: 10`, `cooldown_ms: 2000`.

## Architecture

```text
config      YAML + env loading, defaults
messages    Anthropic Messages parsing, validation, image→text rewriting, context extraction
cache       content-hash (sha256) key + thread-safe LRU + optional SQLite cold layer (TwoTier)
vision      VisionProvider interface + MiMo / OpenAI-compatible / GLM-free / Qwen-VL presets + provider pool + circuit breaker
proxy       request pipeline: parse → find images → cache → describe → replace → forward
logging     structured JSON logs, async writer, request IDs
metrics     Prometheus registry
cli         subcommands: setup / doctor / connect / disconnect / status / stop / version / cache
admin       /admin/shutdown graceful-shutdown endpoint (token-authed)
modelutil   model-name sanitization ([1m] stripping)
buildinfo   build version (ldflags injection)
```

Request path: parse → scan image blocks → hash lookup in LRU → miss → `singleflight` dedup → vision model describes → replace image with text → append system instruction → forward upstream → stream response.

Two things make the proxy cheap:

- **Stateless by design.** Only the current request is processed; the cache is a pure hash→description map that never influences behavior decisions. (This is deliberate: earlier approaches that kept cross-request conversation state were abandoned upstream because behavior got hijacked by history.)
- **Dedup at two levels.** The LRU kills repeated-send costs; `singleflight` collapses concurrent identical-image calls into one. Both key off the image content hash.

The core concurrency follows Go's practical model rather than the slogan: channels for data flow (async log writer, `singleflight` result handoff, shutdown signal), mutexes/atomics where state is actually shared (LRU, counters). `go test -race` is clean.

## Observability

- **JSON logs** — every stage logged with `stage`, `node_name`, `request_id`, and timing fields. Vision calls break down `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms` so you can see dedup wait vs. actual upstream time.
- **`/metrics`** — Prometheus: HTTP requests/durations, images processed, vision calls/durations, upstream requests, cache hit ratio, adaptive-limit gauges.
- **`/healthz`** — liveness probe.
- **`X-Blind-Llm-Eyes` header** — `rewritten=N cached=M` per response.

## Limitations

- **Anthropic Messages format only** (no OpenAI Chat Completions input).
- `/healthz` has no auth — expose only locally. `/metrics` has **optional** token auth (`metrics_auth_token` / `BLIND_METRICS_AUTH_TOKEN`); enable it when exposing beyond localhost.
- In-memory cache by default — descriptions are lost on restart. Opt in to `cache.type: twotier` for SQLite-backed persistence.
- SQLite count drift: the in-memory row counter (`memory_count`) may diverge from the actual DB row count (`actual_count`) if writes fail or an external writer modifies the database. Run `blind-llm-eyes cache stats` periodically to monitor drift; a >5% warning is printed and the observation can trigger a rebuild.
- Self-referential upstream URLs are blocked at 5 layers (import filter / setup reject / doctor FAIL / NewHandler panic / runtime 508). DNS FQDN forms like `127.0.0.1.:8790` are detected (trailing dots stripped). If the proxy's own listen address changes at runtime *after* startup, the last-resort runtime 508 guard still applies.

## Development

```bash
make test          # go test -race -count=1 ./...  — matches the ci.yml CI gate
make vet           # go vet ./...
make build         # local binary with version ldflags
make snapshot      # goreleaser build --snapshot — compile all platform targets
make goreleaser-check  # validate .goreleaser.yaml
```

The `ci` GitHub Actions workflow (`.github/workflows/ci.yml`) runs `go vet` + `go test -race` on a matrix of ubuntu-latest / macos-latest / windows-latest for every push and pull request, plus `go build` on ubuntu-latest. The `make test` command above reproduces the exact race-check gate locally.

Releasing is tag-driven: push a `v*` tag and the `release` workflow runs `goreleaser release`, publishing 6 archives (Windows/Linux/macOS, amd64 + arm64) + `checksums.txt` to the GitHub release. Maintainers can also run `make release` locally with `GITHUB_TOKEN` set.

The test suite covers parsing/rewrite round-trips (including preserving unknown fields), LRU behavior, vision client against a mock server, the full handler pipeline with mock vision + upstream, concurrency bounds, `singleflight` dedup across requests, adaptive-limit behavior, TwoTier sharded-lock parallelism, SQLite count-drift observability, and self-referential URL detection.

### Git Hooks

A pre-commit hook blocks hard-coded API keys (`sk-*` and other patterns) from entering git history.

**Linux/macOS/Git Bash:**
```bash
git config core.hooksPath .githooks
```

**Windows (PowerShell):**
If you use Git Bash, the bash hook works automatically. For native PowerShell,
copy the PowerShell version and ensure the execution policy allows local scripts:
```powershell
git config core.hooksPath .githooks
# If hooksPath doesn't support .ps1, use:
Copy-Item .githooks/pre-commit.ps1 .git/hooks/pre-commit
```

## Roadmap

- Global cross-request concurrency / upstream rate limiting
- Weighted load balancing + active health checks (multi-provider scenarios)

## License

MIT
