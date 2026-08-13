# Multi-Vision Provider Pool & Failover — Design Spec

> **Date:** 2026-08-12
> **Status:** Approved
> **Phase:** P3
> **Prerequisite:** VisionProvider interface abstraction (done), AIMD adaptive concurrency (done)

## 1. Goal

Add a provider pool layer that manages multiple vision providers with priority-based
failover, passive health monitoring via circuit breakers, and per-provider observability —
without modifying the handler's singleflight/errgroup/AIMD core.

## 2. Decisions (confirmed with user)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Provider scope | MiMo (primary) + generic OpenAI-compatible (fallback) | Minimal code, max flexibility — user fills in GPT-4o/GLM-4V/Qwen-VL endpoint |
| Scheduling | Priority-based failover | Backup only called during outages; cost-controllable |
| Health check | Passive observation (no probe goroutine) | Zero probe cost; fault detection delayed by N requests |
| Cache semantics | Provider-agnostic (no key change) | Cache hit across providers; description style variance negligible for downstream text LLM |
| Architecture | Pool implements VisionProvider interface | Zero handler.go changes; pool is transparent to singleflight/errgroup/AIMD |

## 3. Architecture

```
handler.go (UNCHANGED — singleflight + errgroup + AIMD intact)
  └─ h.deps.VisionProvider.DescribeImage(ctx, base64Data, mediaType, imageSize)
       │
       ▼
     Pool.DescribeImage(...)            ← implements VisionProvider
       │  iterate providers by priority (asc)
       ├─ providers[0]: MiMo Client
       │    cb.Allow()? no  → skip
       │    cb.Allow()? yes → call; success → cb.RecordSuccess, return
       │                       fail    → cb.RecordFailure, continue
       ├─ providers[1]: OpenAI Client
       │    (same logic)
       └─ all exhausted → return error (handler fail_open replaces image with placeholder)
```

### Key interaction properties

- **Singleflight:** Pool.DescribeImage is called inside the existing `sf.Do(hash, fn)`.
  Same-hash dedup happens before the pool. Failover happens within one sf call.
- **AIMD:** Handler records `fnExecMs` = total pool call time (including failover).
  Failover = slow sample → AIMD backs off. Correct behavior.
- **Cache:** Provider-agnostic. If cache hits, pool is never called (handler checks
  cache before calling VisionProvider, unchanged). Failover does not invalidate cache.

## 4. New Files

| File | Purpose |
|------|---------|
| `vision/circuit_breaker.go` | 3-state circuit breaker (Closed/Open/Half-open), thread-safe |
| `vision/circuit_breaker_test.go` | State transition + concurrency tests |
| `vision/openai_client.go` | Generic OpenAI-compatible client (`/v1/chat/completions` + `image_url`) |
| `vision/pool.go` | Provider pool with priority failover; implements VisionProvider |
| `vision/pool_test.go` | Priority ordering, failover, skip-open, all-fail tests |

## 5. Modified Files

| File | Change |
|------|--------|
| `config/loader.go` | Add `VisionProviders []ProviderCfg`, `CircuitBreakerCfg`, `ProviderCfg` structs; parsing + defaults + validation (additive) |
| `main.go` | Build provider list from config, construct Pool, inject as VisionProvider |
| `metrics/metrics.go` | Add 4 per-provider metrics (additive) |
| `config.example.yaml` | Add commented-out `vision_providers:` example |

## 6. Circuit Breaker Design

### States

```
Closed ──(consecutive fails >= failure_threshold)──▶ Open
  ▲                                                    │
  │                                                    │ (after reset_timeout)
  └──(trial success)──── Half-open ◀──────────────────┘
                              │
                          (trial fail)
                              ▼
                            Open (restart reset_timeout timer)
```

- **Closed:** Normal operation. Count consecutive failures. Any success resets count to 0.
  When count >= `failure_threshold` → transition to Open, record `openedAt`.
- **Open:** `Allow()` returns false (deny). After `reset_timeout` elapses since `openedAt`,
  transition to Half-open on next `Allow()` call.
- **Half-open:** Only ONE trial request allowed (atomic flag `halfOpenTrialInFlight`).
  Success → Closed (reset failure count). Failure → Open (restart timer).

### Thread safety

All state transitions guarded by `sync.Mutex`. Half-open single-trial enforced by
a bool flag set under lock — if a trial is in flight, other callers are denied
(they skip to the next provider or fail).

### Config defaults

- `failure_threshold`: 5 (consecutive failures to open)
- `reset_timeout`: 30s (open → half-open wait)

## 7. Pool Design

### Initialization (`NewPool`)

1. Validate config: >= 1 provider; each has name/type/base_url/api_key/model
2. For each `ProviderCfg`, construct the appropriate client via `buildProvider()`:
   - `type: "mimo"` → `vision.NewClient` (existing, Anthropic Messages format)
   - `type: "openai_compatible"` → `vision.NewOpenAIClient` (new)
3. Build `providerEntry` for each: name, provider, priority, circuit breaker
4. Stable-sort entries by priority (asc); ties preserve config order
5. Return `*Pool`

### `DescribeImage` flow

```
for each providerEntry (sorted by priority):
    allowed, release := entry.cb.Allow()
    if not allowed:
        record metric (skipped)
        continue
    desc, err := entry.provider.DescribeImage(ctx, ...)
    release()  // release half-open trial slot
    if err == nil:
        entry.cb.RecordSuccess()
        record metric (success, duration)
        return desc, nil
    entry.cb.RecordFailure()
    record metric (error, duration)
    log failover event
return "", error  // all providers failed or open
```

### Backward compatibility

- `vision_providers:` absent → single-provider mode (today's behavior, no pool overhead)
- `vision_providers:` present → pool mode (old `vision:` block ignored)
- In single-provider mode, `main.go` constructs `Client` directly as today

## 8. Config Structure

### Single-provider mode (backward compatible, unchanged)

```yaml
vision:
  base_url: "https://api.xiaomimimo.com/anthropic"
  api_key: "sk-..."
  model: "mimo-v2.5"
  # ... all existing fields
```

### Multi-provider mode (new, opt-in)

```yaml
vision_providers:
  - name: "mimo"
    type: "mimo"                     # Anthropic Messages API format
    priority: 1
    base_url: "https://api.xiaomimimo.com/anthropic"
    api_key: "sk-..."
    model: "mimo-v2.5"
    timeout: "60s"
    large_image_timeout: "120s"
    large_image_threshold: 1000000
    description_cap: 1000
    supported_formats: ["image/png", "image/jpeg", "image/webp", "image/gif"]
    circuit_breaker:
      failure_threshold: 5
      reset_timeout: "30s"

  - name: "openai-fallback"
    type: "openai_compatible"        # OpenAI Chat Completions format
    priority: 2
    base_url: "https://api.openai.com/v1"
    api_key: "sk-..."
    model: "gpt-4o"
    timeout: "30s"
    large_image_timeout: "60s"
    large_image_threshold: 1000000
    description_cap: 1000
    supported_formats: ["image/png", "image/jpeg", "image/webp"]
    circuit_breaker:
      failure_threshold: 3
      reset_timeout: "30s"
```

### Config structs

```go
type ProviderCfg struct {
    Name                string            `yaml:"name"`
    Type                string            `yaml:"type"` // "mimo" | "openai_compatible"
    Priority            int               `yaml:"priority"`
    BaseURL             string            `yaml:"base_url"`
    APIKey              string            `yaml:"api_key"`
    Model               string            `yaml:"model"`
    TimeoutStr          string            `yaml:"timeout"`
    LargeTimeoutStr     string            `yaml:"large_image_timeout"`
    LargeImageThreshold int64             `yaml:"large_image_threshold"`
    DescriptionCap      int               `yaml:"description_cap"`
    SupportedFormats    []string          `yaml:"supported_formats"`
    CircuitBreaker      CircuitBreakerCfg `yaml:"circuit_breaker"`
}

type CircuitBreakerCfg struct {
    FailureThreshold int    `yaml:"failure_threshold"`
    ResetTimeoutStr  string `yaml:"reset_timeout"`
}
```

### Defaults (in `config.Load`)

- `type`: `"mimo"` (if empty)
- `failure_threshold`: 5
- `reset_timeout`: `"30s"`
- `description_cap`: 1000
- `large_image_threshold`: 1000000
- `supported_formats`: `["image/png", "image/jpeg", "image/webp", "image/gif"]`
- `timeout`: `"60s"`
- `large_image_timeout`: `"120s"`

### Validation

- `vision_providers` must have >= 1 entry when present
- Each provider needs: `name`, `type` (one of the two), `base_url`, `api_key`, `model`
- `failure_threshold >= 1`
- Priorities need not be unique (ties broken by list order)

## 9. OpenAI-Compatible Client

`vision/openai_client.go` implements `VisionProvider` via OpenAI Chat Completions format:

- Endpoint: `{base_url}/chat/completions`
- Request body: `messages` with `image_url` content type (`data:{media_type};base64,{data}`)
- Response: parse `choices[0].message.content` (string)
- Reuses existing `convertWebPToPNG` for WebP support
- Same `VisionProvider` interface, same `DescribeImage` signature
- Struct fields mirror `Client` (BaseURL, APIKey, Model, timeouts, etc.)

## 10. Metrics (additive)

New Prometheus metrics (existing ones unchanged):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `blind_llm_eyes_provider_calls_total` | Counter | `provider`, `result` | Per-provider calls; result ∈ `success\|error\|skipped` |
| `blind_llm_eyes_provider_duration_seconds` | Histogram | `provider` | Per-provider latency |
| `blind_llm_eyes_circuit_breaker_state` | Gauge | `provider` | 0=closed, 1=open, 2=half-open |
| `blind_llm_eyes_failover_events_total` | Counter | — | Incremented on each failover |

Existing pool-level metrics (`vision_calls_total`, `vision_call_duration_seconds`) unchanged —
handler records them at the pool level (it sees pool as one VisionProvider).

## 11. Testing

| Test file | Coverage |
|-----------|----------|
| `vision/circuit_breaker_test.go` | Closed→Open (threshold), Open→Half-open (timeout), Half-open→Closed (trial success), Half-open→Open (trial fail), concurrent half-open single-trial, reset_timeout timing |
| `vision/pool_test.go` | Priority ordering, failover on error, skip open breaker, all-fail returns error, success does not failover, metrics recording, mock providers via interface |
| `config/loader_test.go` | Multi-provider parsing, defaults applied, validation errors, backward compat (single `vision:` block still works) |
| Existing tests | `go test -race ./...` stays green (handler untouched) |

## 12. Non-goals (YAGNI)

- Active health check probes (passive only)
- Weighted load balancing (priority failover only)
- Per-provider AIMD controllers (single AIMD at pool level)
- Provider-specific rate limiting
- Dynamic provider add/remove at runtime
- Config hot-reload

## 13. Compatibility with existing production config

The current production config (`concurrency_limit=6`, `adaptive [1,12]`,
`sample_window=10`, `cooldown=2s`) is completely untouched. These live in separate
top-level keys and apply identically in both single-provider and multi-provider modes.
The pool only replaces vision client construction, not concurrency governance.
