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

## Quick start

### 1. Build

```bash
go build -o blind-llm-eyes .
```

### 2. Configure

```bash
cp config.example.yaml config.yaml   # then fill in real keys
```

Minimal working config (real values used in production):

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

### 3. Run

```bash
./blind-llm-eyes -config config.yaml
```

### 4. Point Claude Code at it

Set the provider's `ANTHROPIC_BASE_URL` to `http://127.0.0.1:8790` (via env override in CC Switch, or the provider's base URL setting), then paste a screenshot. The text-only model should now answer questions about the image.

Verify with a single request:

```bash
curl -N http://127.0.0.1:8790/v1/messages \
  -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","max_tokens":500,"stream":true,"messages":[{"role":"user","content":[
    {"type":"text","text":"What is in this image?"},
    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<base64>"}}]}]}'
```

Response header `X-Blind-Llm-Eyes` reports the outcome: `rewritten=1 cached=0`.

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
go build ./...     # compile
go vet ./...       # static checks
go test -race ./...  # tests with race detector
```

The test suite covers parsing/rewrite round-trips (including preserving unknown fields), LRU behavior, vision client against a mock server, the full handler pipeline with mock vision + upstream, concurrency bounds, `singleflight` dedup across requests, and adaptive-limit behavior.

## Roadmap

- Nested `tool_result` image support (protocol correctness for real Claude Code traffic)
- Conversation-context-aware descriptions (feed recent messages to the vision model for intent-aware descriptions)
- Global cross-request concurrency / upstream rate limiting

## License

MIT
