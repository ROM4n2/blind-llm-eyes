# Release Notes — v1.0.0

> ← Back to [README](README.md) · [HANDOFF](HANDOFF.md) · [CHANGELOG-style commits](../commits/v1.0.0)

**blind-llm-eyes** — Give text-only LLMs eyes.

Release date: 2026-08-13
Branch: merged to `master` via `58c1ee8` (tag: `v1.0.0`)
Scope: 13 commits · 44 files changed · +5379 / −83 lines (incl. docs)

---

## Overview

v1.0.0 transitions blind-llm-eyes from a development prototype into a productized, user-installable tool. The proxy core (image→description rewriting, caching, concurrency, observability) was already in place from earlier milestones; this release adds everything a non-developer needs to **install, configure, run, and manage** the proxy as a daily driver alongside Claude Code — without touching a Go toolchain.

The headline change: a single static binary with a guided `setup` wizard, one-command `connect` to Claude Code, and `start`/`stop`/`status`/`doctor` lifecycle commands. Precompiled binaries for Windows / Linux / macOS (amd64 + arm64) are published automatically on every `v*` tag via goreleaser.

### User flow (new in v1.0.0)

```
download binary → blind-llm-eyes setup → blind-llm-eyes connect → blind-llm-eyes start
                                                                        ↳ status / stop / doctor
```

---

## New features

### 1. CLI subcommand system (`blind-llm-eyes <subcommand>`)

A thin dispatch layer in [main.go](file:///d:/Code/new-api-contrib/main.go) routes to subcommands implemented in the new [cli/](file:///d:/Code/new-api-contrib/cli) package. Running with no args still starts the server (backward compat with pre-v1.0.0 invocation).

| Subcommand | Purpose |
| --- | --- |
| `version` | Print version + Go runtime (`buildinfo.Version`, injected via ldflags at release time) |
| `setup` | Interactive config wizard (see §2) |
| `connect` | Wire Claude Code's `settings.json` to the proxy (see §3) |
| `disconnect` | Restore `settings.json` from backup |
| `start` | Run the proxy (default if no subcommand given) |
| `stop` | Graceful shutdown via admin endpoint (see §4) |
| `status` | Check pidfile + `/healthz` → `RUNNING` / `STALE` / `NOT RUNNING` |
| `doctor` | Connectivity self-check: ping upstream + each vision provider (see §5) |

### 2. Interactive setup wizard (`setup`)

[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go) orchestrates the full first-run experience:

1. **cc-switch import** *(optional)* — reads `~/.cc-switch/cc-switch.db` (SQLite), lists all Claude Code providers, lets you pick which to use as upstream and vision. Models are sanitized on import (see §6).
2. **Manual input with defaults** — base URL / API key / model for upstream and vision, with sensible defaults (`https://api.deepseek.com/anthropic`, `mimo-v2.5`, etc.).
3. **Doctor self-check** — pings both endpoints before saving; on failure, asks whether to save anyway.
4. **Config generation** — writes `config.yaml`.
5. **Optional connect** — offers to run `connect` immediately.
6. **Startup instructions** — prints the exact commands to run next.

### 3. Claude Code wiring (`connect` / `disconnect`)

[cli/connect.go](file:///d:/Code/new-api-contrib/cli/connect.go) + [cli/settings.go](file:///d:/Code/new-api-contrib/cli/settings.go)

- `connect` rewrites `~/.claude/settings.json`'s `env.ANTHROPIC_BASE_URL` to `http://127.0.0.1:8790`.
- A full backup is written to `~/.claude/.bak-before-connect` **before** any modification. Repeated `connect` calls update the URL but never overwrite the backup (so the original state is always recoverable).
- Atomic write: the new `settings.json` is written to a temp file then renamed, so a crash mid-write can't corrupt the file.
- `disconnect` restores `settings.json` byte-for-byte from the backup and removes the backup marker.
- Both commands auto-detect the settings path per OS (`%USERPROFILE%\.claude` on Windows, `~/.claude` elsewhere).

### 4. Process lifecycle: admin shutdown + pidfile

[admin/admin.go](file:///d:/Code/new-api-contrib/admin/admin.go) + [cli/pidfile.go](file:///d:/Code/new-api-contrib/cli/pidfile.go) + [cli/status.go](file:///d:/Code/new-api-contrib/cli/status.go) + [cli/stop.go](file:///d:/Code/new-api-contrib/cli/stop.go)

- On `start`, the server writes a pidfile (`pidfile.json`) containing PID, listen address, shutdown token, and start time.
- `POST /admin/shutdown` with `X-Admin-Token: <token>` triggers a graceful drain (waits for in-flight requests via the existing `WaitGroup`), then signals `main` to exit. Wrong/missing token → `403 Forbidden`. Correct token → `202 Accepted`.
- `stop` reads the pidfile, sends the authenticated shutdown request, and removes the pidfile.
- `status` cross-checks the pidfile against `/healthz`: reports `RUNNING` (pidfile + healthz OK), `STALE` (pidfile exists but healthz unreachable), or `NOT RUNNING` (no pidfile).

### 5. Connectivity doctor (`doctor`)

[cli/doctor.go](file:///d:/Code/new-api-contrib/cli/doctor.go) + [vision/ping.go](file:///d:/Code/new-api-contrib/vision/ping.go) + [cli/ping_upstream.go](file:///d:/Code/new-api-contrib/cli/ping_upstream.go)

- **Vision ping** (`vision.Ping`): lightweight `POST /v1/messages` with a 1×1 PNG and `max_tokens=1`. Verifies both reachability and authentication without sending a real image. Implemented on the client, the single provider, and the pool (pings each provider, reports per-provider status).
- **Upstream ping** (`cli.PingUpstream`): `POST /v1/messages` with a trivial text message; checks the HTTP status and that a parseable response comes back.
- `doctor` runs both, prints a pass/fail table, and exits non-zero if any check fails — usable in scripts.

### 6. Model name sanitization (`[1m]` stripping)

[modelutil/modelutil.go](file:///d:/Code/new-api-contrib/modelutil/modelutil.go)

Some vendor UIs (e.g. cc-switch) append a context-length suffix to model names — `deepseek-chat[1m]`, `deepseek-chat[1M]`. Upstream APIs reject these as unknown models. `modelutil.SanitizeModel` strips a trailing `[<digits><unit>]` suffix (case-insensitive unit) before the handler forwards the request. Integrated into the proxy handler, the cc-switch import path, and the setup wizard.

### 7. cc-switch SQLite import

[cli/ccswitch.go](file:///d:/Code/new-api-contrib/cli/ccswitch.go)

- Reads the `providers` table (`app_type = 'claude'`) from `~/.cc-switch/cc-switch.db`.
- Opens the database **read-only**; if the file is locked (cc-switch GUI running), falls back to copying the DB to a temp file.
- Parses each provider's `settings_config` JSON, extracts `env.ANTHROPIC_BASE_URL` / `ANTHROPIC_API_KEY` / `ANTHROPIC_MODEL`, sanitizes the model name.
- Malformed rows are silently skipped; the import never fails the whole setup.

---

## Improvements

### Refactored provider construction

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go)

Inline provider construction was extracted from `main.go` into reusable, testable functions:
- `BuildProvider(cfg)` — builds a single `vision.Client` from config.
- `BuildSingleProvider(cfg)` — wraps it as a `*SingleProvider` with circuit-breaker stats.
- `BuildPool(cfg)` — builds a multi-provider pool with failover.

This cut `main.go` from ~200 lines of wiring to a thin dispatch + server lifecycle, and made provider construction unit-testable.

### README quick start rewrite

[README.md](file:///d:/Code/new-api-contrib/README.md) Quick start now documents the productized flow (download → `setup` → `connect` → `start` → `status`/`stop`/`doctor`) instead of the old `go build` + manual `cp config.example.yaml` path. A Development section documents the new `make` targets.

---

## Testing

### E2E integration test suite (new)

[test/e2e_test.go](file:///d:/Code/new-api-contrib/test/e2e_test.go) — 572 lines, 5 test cases, all passing under `-race`.

| Test | What it verifies |
| --- | --- |
| `TestE2E_FullPipeline` | Real `vision.Client` + real `proxy.NewHandler` against httptest fakes. Sends `deepseek-chat[1m]` + image block; asserts `[1m]` stripped before upstream, exactly 1 vision call, vision sees `mimo-v2.5`, image replaced by description, SSE passthrough, `X-Blind-Llm-Eyes` header, 2nd request cache hit. |
| `TestE2E_AdminShutdown_PidfileCleanup` | Real `admin.ShutdownHandler` + real `cli.WritePidfile`/`ReadPidfile`. Wrong token → 403 (no shutdown). Correct token → 202 + `Done()` closed + pidfile removed. |
| `TestE2E_AdminShutdown_RejectsMissingToken` | Tokenless POST → 403, handler still armed. |
| `TestE2E_VisionTimeout_FailOpen` | Slow vision server (2s delay) + 200ms client timeout + `FailOpen=true` → 200 response, upstream receives placeholder `[Image could not be described by vision model]`, not the image or delayed response. |
| `TestE2E_VisionTimeout_FailClosed` | Same slow server + `FailOpen=false` → 502 response, upstream never reached, body mentions `vision call failed`. |

The timeout tests use a `select`+`done` channel pattern (not bare `time.Sleep`) so `httptest.Server.Close()` doesn't block on the hanging handler — a goroutine-lifecycle lesson from prior integration-test work.

### TDD coverage

Every new subcommand and package was developed test-first. New test files: `admin/admin_test.go`, `buildinfo/buildinfo_test.go`, `cli/*_test.go` (8 files), `modelutil/modelutil_test.go`, `proxy/handler_modelutil_test.go`, `vision/ping_test.go`, `vision/provider_test.go`, `test/e2e_test.go`.

**Full suite: `go test -race -count=1 ./...` green across all 13 packages.**

---

## Release infrastructure

### goreleaser

[.goreleaser.yaml](file:///d:/Code/new-api-contrib/.goreleaser.yaml)

- Cross-compiles 6 targets: `linux/darwin/windows × amd64/arm64`.
- `CGO_ENABLED=0` — all dependencies are pure Go (including `modernc.org/sqlite` for cc-switch import), so the cross-compile is hermetic on any runner.
- Injects `buildinfo.Version` via ldflags (`-X ...buildinfo.Version={{.Version}}`).
- Produces `.tar.gz` (linux/darwin) and `.zip` (windows) archives + `checksums.txt`.
- Git-based changelog, excluding `docs:`/`test:`/`chore:`/merge commits.

Verified locally: `goreleaser check` passes; `goreleaser build --snapshot` produces all 6 binaries (~15 MB each) with correct version injection.

### GitHub release workflow

[.github/workflows/release.yml](file:///d:/Code/new-api-contrib/.github/workflows/release.yml)

Tag-driven: push a `v*` tag → the workflow checks out with full history, sets up Go from `go.mod`, and runs `goreleaser release --clean`. Publishes archives + checksums to the GitHub release. `permissions: contents: write` is scoped to the workflow.

### Makefile

[Makefile](file:///d:/Code/new-api-contrib/Makefile)

Dev convenience targets:

| Target | Action |
| --- | --- |
| `make test` | `go test -race -count=1 ./...` (the CI gate) |
| `make vet` | `go vet ./...` |
| `make build` | Local binary with version ldflags (`VERSION ?= dev`) |
| `make snapshot` | `goreleaser build --snapshot --clean` (all 6 targets, no publish) |
| `make goreleaser-check` | Validate `.goreleaser.yaml` |
| `make release` | `goreleaser release --clean` (maintainer-only, needs `GITHUB_TOKEN`) |
| `make clean` | Remove `dist/` and local binary |

---

## Upgrade / migration notes

This is the first tagged release; there is no in-place upgrade path. For existing dev-checkout users:

1. Pull the `feat/onboarding-productize` branch (or the `v1.0.0` tag once cut).
2. Either build from source (`make build`) or download a precompiled binary from the releases page.
3. Run `blind-llm-eyes setup` — it will detect your existing `config.yaml` defaults but walk you through validation. Existing `config.yaml` files remain compatible; no schema changes.
4. If you previously set `ANTHROPIC_BASE_URL` manually in Claude Code's settings or cc-switch, `blind-llm-eyes connect` will now manage it for you (with a backup).

**Breaking invocation change:** none. `blind-llm-eyes -config config.yaml` still starts the server (the no-subcommand path is preserved as `start`).

---

## Commits in this release

```
3f059f8  test(e2e): add network timeout scenarios for fail-open and fail-closed paths
29c5104  test: add e2e integration test for full pipeline and admin shutdown
9f46a2b  chore: add goreleaser, release workflow, and makefile
1508234  feat(cli): add interactive setup wizard with cc-switch import and doctor
252d06e  feat(cli): add cc-switch SQLite provider import
5565bd6  feat(cli): add connect/disconnect for Claude Code settings.json wiring
7a26701  feat(proxy): strip [1m] model suffix before forwarding to upstream
e2cbf8b  feat(cli): add doctor subcommand with vision and upstream ping
405b4b2  feat(admin): add shutdown endpoint, pidfile, and status/stop subcommands
77f8ee8  feat(cli): add subcommand dispatch skeleton and thin main.go
ad70a9f  refactor(vision): extract provider builders from main.go
3d9f9eb  feat(cli): add buildinfo package and version subcommand
```

---

## Known limitations

Carried over from pre-v1.0.0 (not introduced by this release):

- **Top-level image blocks only.** Images nested inside `tool_result` blocks pass through undescribed — next planned change.
- **Anthropic Messages format only** (no OpenAI Chat Completions input).
- **In-memory cache** — descriptions are lost on restart.
- No client auth on `/metrics` or `/healthz` — expose only locally.
- **CC Switch proxy mode** truncates image bodies; users must set `ANTHROPIC_BASE_URL` directly (the `connect` subcommand does this correctly).

---

## Verification

- `go test -race -count=1 ./...` — 13 packages, all green
- `go vet ./...` — clean
- `goreleaser check` — config valid
- `goreleaser build --snapshot --clean` — 6 binaries built, version ldflags confirmed (`blind-llm-eyes 0.0.1-next (go go1.26.5)`)
- `blind-llm-eyes version` — prints injected version + Go runtime
