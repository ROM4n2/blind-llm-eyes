# Release Notes — v1.0.1

> ← Back to [README](README.md) · [v1.0.0 notes](RELEASE_NOTES-v1.0.0.md)

**blind-llm-eyes** — Give text-only LLMs eyes.

Release date: 2026-08-14
Scope: 7 commits since `v1.0.0` — 6 Tier-1 fixes + docs

---

## Overview

v1.0.1 is the first patch release. It fixes the two most urgent bugs reported
against v1.0.0 — a broken token counter in Claude Code and CLI commands failing
under the Trae IDE sandbox — and adds four quality-of-life improvements: a
vision-model whitelist passthrough, a free GLM-4V-Flash onboarding preset,
double-click launcher scripts in the release archives, and a `doctor --deep`
end-to-end image test.

No breaking changes. Existing `config.yaml` files remain compatible; new fields
are optional with sensible defaults.

---

## Bug fixes

### 1. `count_tokens` endpoint passthrough

[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go)

Claude Code calls `POST /v1/messages/count_tokens` to populate its token
counter. v1.0.0 returned `404` for this path, leaving the counter blank. The
handler now registers the route and forwards the request body verbatim to the
upstream, streaming the response back untouched. No vision rewriting, no
caching — a pure transparent proxy.

### 2. Trae IDE sandbox: pidfile & settings writes

[cli/pidfile.go](file:///d:/Code/new-api-contrib/cli/pidfile.go) ·
[cli/settings.go](file:///d:/Code/new-api-contrib/cli/settings.go)

Under the Trae IDE sandbox, `os.CreateTemp` is blocked in protected directories
(`%AppData%`, `~/.claude/`), so `status`, `stop`, and `disconnect` failed. The
atomic-write helpers now write to a fixed-name temp file (`<path>.tmp`) via
`os.WriteFile` then `os.Rename` — same atomicity guarantee, no
`CreateTemp` call. The `disconnect no-backup` test was moved off the real
`~/.claude/` path to a temp dir for determinism.

---

## New features

### 3. Vision-capable model whitelist (passthrough)

[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go)

When the upstream model natively accepts images (e.g. `gpt-4o`), rewriting them
into text is wasteful — it burns a vision API call and adds ~8s latency for no
benefit. A new `vision_capable_models` set (case-insensitive) lets the handler
skip the entire rewrite phase and forward the body verbatim when the sanitized
model matches. The response carries an `X-Blind-Llm-Eyes` passthrough marker.
Empty/nil set = never skip (the default, unchanged behavior).

### 4. Free GLM-4V-Flash preset

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go) ·
[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go)

A new `glm_free` provider type auto-fills the GLM-4V-Flash base URL and model
(Zhipu AI BigModel platform). The model is free to use — only a (free) API key
from `https://open.bigmodel.cn` is required. The `setup` wizard now offers it as
the default vision provider, removing the payment barrier for first-run
onboarding.

### 5. Double-click launcher scripts in release archives

[.goreleaser.yaml](file:///d:/Code/new-api-contrib/.goreleaser.yaml) ·
[scripts/](file:///d:/Code/new-api-contrib/scripts)

Each release archive now ships `start.bat` (Windows), `start.sh` (Linux), and
`start.command` (macOS) at the root. Double-clicking runs
`blind-llm-eyes start` and keeps the window open on exit — no terminal needed.

### 6. `doctor --deep` end-to-end image test

[cli/doctor.go](file:///d:/Code/new-api-contrib/cli/doctor.go)

The `--deep` flag sends a real 1×1 PNG through `DescribeImage` after the text
ping passes, verifying the full vision pipeline (base64 decode, API call,
response parsing). A non-empty description is required to pass. Catches
misconfigurations that a text-only ping can't (wrong media type handling,
truncated responses, empty-description bugs).

---

## Commits in this release

```text
103f190 feat(cli): add doctor --deep end-to-end vision pipeline test
1195927 chore(release): add start scripts to release archives
27217bb docs: add performance test results and known bugs
4485c37 feat(vision): add glm_free provider type for GLM-4V-Flash free tier
0448c7e feat(proxy): add vision-capable model whitelist passthrough
eb222a3 fix(cli): avoid trae sandbox temp file restriction in pidfile and settings
6bac77c feat(proxy): add count_tokens endpoint passthrough
```

---

## Verification

- `go test -race -count=1 ./...` — all packages green
- `go vet ./...` — clean
- `go build ./...` — clean

---

## Upgrade notes

Drop-in replacement for v1.0.0. To use the new opt-in features:

- **Whitelist passthrough** — add to `config.yaml`:
  ```yaml
  vision_capable_models: ["gpt-4o", "gpt-4-turbo"]
  ```
  (or per-provider `vision_capable_models` in `vision_providers[]`). Omit to
  keep the always-rewrite behavior.

- **GLM-4V-Flash free tier** — re-run `blind-llm-eyes setup` and pick option 1,
  or set a provider `type: glm_free` with just an `api_key`.

- **doctor --deep** — `blind-llm-eyes doctor --deep`
