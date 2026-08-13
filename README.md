# blind-llm-eyes

**English** | [中文](README.zh-CN.md)

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
- **Content-hash LRU cache** — the same image re-sent across turns triggers zero vision calls (the typical multi-turn resend case).
- **`singleflight` in-flight dedup** — concurrent requests carrying the same image share a single vision call.
- **Parallel image processing** — images in one request are described concurrently via `errgroup`, bounded by `concurrency_limit`.
- **Adaptive concurrency** *(optional)* — AIMD-style controller that raises/lowers the concurrency limit from real vision latency feedback (P90 + error rate), protecting against slow upstreams.
- **fail-open** — a failed vision call replaces the image with a placeholder instead of blocking the whole request.
- **WebP → PNG conversion** — automatically converts WebP images before sending them to the vision model.
- **Adaptive timeouts** — large images get a longer timeout (`large_image_timeout`).
- **Observability** — structured JSON logs (async writer), per-stage timing via `httptrace`, Prometheus metrics at `/metrics`, request IDs threaded through the whole pipeline, graceful shutdown.
- **Pluggable vision backends** — anything implementing `vision.VisionProvider` works.
- **Single static binary** — no runtime dependencies, ~10 MB.
- **Model name sanitization** — automatically strips vendor context-length suffixes before forwarding to upstream (`deepseek-chat[1m]` → `deepseek-chat`); applied both on the request path and during cc-switch import (double-safe).
- **CLI lifecycle** — 8 subcommands cover the full lifecycle: `setup` (interactive config + doctor), `doctor` (connectivity self-check), `connect`/`disconnect` (Claude Code settings.json wiring), `start`, `status`, `stop`, `version`.
- **cc-switch one-click import** — reads providers directly from the cc-switch SQLite database (best-effort: falls back to temp-copy on DB lock, falls back to manual input on any error).
- **Safe settings management** — `connect` rewrites Claude Code's `settings.json` with a full-file backup taken exactly once (repeat `connect` never overwrites the backup); `disconnect` restores byte-for-byte from the backup via atomic writes.

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
|---|---|---|
| `doctor` reports vision `PASS` but L4 returns 502 with `"vision call failed"` | Real `DescribeImage` (larger payload + longer timeout) fails where `Ping` (1-token) succeeds. Common when vision timeout is too small. | Raise `vision.timeout` (default 30s) and confirm the vision model accepts images at its configured endpoint. |
| `status` returns `NOT RUNNING` even though `start` is running in foreground | On Windows Trae IDE terminals, `os.CreateTemp` for the pidfile is blocked by the sandbox (sandbox error: `Not allow operate files: ...pidfile-*.tmp`). This only affects the IDE-integrated terminal. | Run `status` / `stop` from a standalone PowerShell window. The foreground `start` itself works everywhere, including inside Trae. |
| Upstream returns 400 `"model: deepseek-chat[1m] not found"` | `[1m]` suffix reached upstream (model sanitization is not active). Older pre-v1.0.0 builds didn't strip the suffix; or a reverse proxy in front of blind-llm-eyes is re-injecting the original model. | Upgrade to v1.0.0+ (`blind-llm-eyes version` confirms). Confirm the `ANTHROPIC_MODEL` env in Claude Code / cc-switch doesn't override the model field sent through the proxy — the sanitization only runs *inside* the proxy on the parsed request body. |
| Claude Code still says "I can't see images" after `connect` | Claude Code reads `settings.json` on startup only; or a cc-switch switcher run after `connect` overwrote `ANTHROPIC_BASE_URL` back. | Restart Claude Code. Run `connect` again and **don't** switch providers in cc-switch while using the proxy. |
| Images get truncated / vision hash mismatch errors | Using CC Switch **proxy mode** (not base_url override). It silently truncates request bodies > ~200 bytes before forwarding. | Set `ANTHROPIC_BASE_URL=http://127.0.0.1:8790` directly. Don't enable proxy mode in cc-switch. `blind-llm-eyes connect` writes exactly this setting. |
| `go install github.com/ROM4n2/blind-llm-eyes@latest` errors with "invalid version: unknown revision" | `@latest` requires at least one published semver tag on the remote repo. Tag `v1.0.0` exists locally but hasn't been pushed yet. | Download a prebuilt archive from the releases page, or build from a checkout: `go build -ldflags "-X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version=1.0.0" .` |

### Security considerations

- **Admin token** — generated fresh from `crypto/rand` on every `start`, written to the pidfile at `<UserConfigDir>/blind-llm-eyes/pidfile.json`, and deleted by `stop`. It's never persisted elsewhere (no env var, no config key).
- **Bind address** — default listen is `127.0.0.1:8790` (loopback only). Exposing `0.0.0.0` or a LAN IP forwards `/v1/messages` with your upstream API key to anyone who can reach the port — never do that on an untrusted network. `/metrics` and `/healthz` also have no client auth.
- **Keys from config vs env** — API keys set in `config.yaml` override the client's `Authorization` header. The handler strips `Authorization` / `Proxy-Authorization` / `Cookie` headers from the client before forwarding upstream whenever `UpstreamAPIKey` is configured, to avoid leaking client-side credentials.
- **Pidfile permissions** — on Windows the pidfile directory defaults to `%AppData%\blind-llm-eyes\` (user-only ACLs inherited from `AppData`). Don't share `%AppData%` across accounts or put it on a world-readable share.
- **`connect` backup** — `~/.claude/.bak-before-connect` is a verbatim copy of your original `settings.json`. It contains whatever secrets `settings.json` held (e.g. `ANTHROPIC_API_KEY`). Treat it the same as the real file.

## Configuration reference

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `127.0.0.1:8790` | Bind address |
| `upstream.base_url` | — (required) | Text-only upstream root (Anthropic-compatible) |
| `upstream.api_key` | — | If set, overrides the client's `Authorization` when forwarding |
| `vision.base_url` | — (required) | Vision model root; the client appends `/v1/messages` |
| `vision.api_key` | — | Vision provider key |
| `vision.model` | — | Vision model name |
| `vision.timeout` | `30s` | Default vision call timeout |
| `vision.large_image_timeout` | `120s` | Timeout for images ≥ `large_image_threshold` |
| `vision.large_image_threshold` | `1048576` | Bytes; images at/above this get the large timeout |
| `vision.description_cap` | `1000` | `max_tokens` for descriptions |
| `vision.supported_formats` | png/jpeg/webp/gif | Allowed media types |
| `cache.max_entries` | `500` | In-memory LRU capacity |
| `concurrency_limit` | `4` | Max parallel vision calls per request; also the adaptive initial value |
| `adaptive_concurrency.*` | disabled | AIMD controller (see below) |
| `fail_open` | `false` | Vision failure → placeholder instead of 502 |
| `log_level` | `info` | `debug`/`info`/`warn`/`error` |

### Adaptive concurrency

Mirrors TCP congestion control. Every real vision call (the `singleflight` executor only, so each sample reflects actual upstream latency) goes into a rolling window; when the window fills, the P90 latency and error rate decide the new limit:

- P90 < `fast_threshold_ms` and no errors → `+increase_step` (additive increase)
- P90 > `slow_threshold_ms` or error rate > `error_threshold` → `×decrease_ratio` (multiplicative decrease)
- otherwise → unchanged (hysteresis band prevents oscillation)

Disabled by default; when disabled, behavior is identical to a static `concurrency_limit`. Production smoke-test tuning (2026-08-12): MiMo averages ~7.7 s, worst 20.6 s, so the defaults are `concurrency_limit: 6`, `max_limit: 12`, `sample_window: 10`, `cooldown_ms: 2000`.

## Architecture

```text
config      YAML + env loading, defaults
messages    Anthropic Messages parsing, validation, image→text rewriting
cache       content-hash (sha256) key + thread-safe LRU
vision      VisionProvider interface + MiMo Anthropic-format client
proxy       request pipeline: parse → find images → cache → describe → replace → forward
logging     structured JSON logs, async writer, request IDs
metrics     Prometheus registry
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

- **Top-level image blocks only.** Images nested inside `tool_result` blocks are passed through untouched (not yet described) — real traffic support for nested tool-result images is the next planned change.
- **Anthropic Messages format only** (no OpenAI Chat Completions input).
- **In-memory cache** — descriptions are lost on restart (acceptable for a personal proxy).
- No client auth on `/metrics` or `/healthz` — expose only locally.

## Development

```bash
make test          # go test -race -count=1 ./...  (the CI gate)
make vet           # go vet ./...
make build         # local binary with version ldflags
make snapshot      # goreleaser build --snapshot — compile all platform targets
make goreleaser-check  # validate .goreleaser.yaml
```

Releasing is tag-driven: push a `v*` tag and the `release` workflow runs `goreleaser release`, publishing archives + checksums to the GitHub release. Maintainers can also run `make release` locally with `GITHUB_TOKEN` set.

The test suite covers parsing/rewrite round-trips (including preserving unknown fields), LRU behavior, vision client against a mock server, the full handler pipeline with mock vision + upstream, concurrency bounds, `singleflight` dedup across requests, and adaptive-limit behavior.

## Roadmap

- Nested `tool_result` image support (protocol correctness for real Claude Code traffic)
- Conversation-context-aware descriptions (feed recent messages to the vision model for intent-aware descriptions)
- Global cross-request concurrency / upstream rate limiting

## License

MIT
