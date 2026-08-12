---
alwaysApply: true
scene: git_message
---

# Git Commit Message Convention

Write every commit message in **English**, in the format:

    <type>(<scope>): <summary>

## Types

| type    | when to use                                   |
|---------|-----------------------------------------------|
| `feat`  | a new feature or capability                   |
| `fix`   | a bug fix                                     |
| `perf`  | a performance improvement                     |
| `test`  | adding or updating tests                      |
| `docs`  | documentation only                            |
| `chore` | maintenance: scaffolding, build, dependencies |

## Scope (optional)

Use a scope only when it pinpoints the changed area. Common scopes in this
repo: `proxy`, `vision`, `messages`, `cache`, `observability`, `logging`,
`main`. Omit the scope for `docs`, `chore`, and cross-cutting `fix`es.

## Summary

- Lowercase, descriptive style, e.g. `add singleflight to dedup in-flight
  vision calls` or `parallelize image processing with errgroup`.
- One change per commit — no "and" chains.
- No trailing period; keep under ~70 characters.
- Prefer natural, action-first phrasing; avoid jargon.

## Body (write for significant changes)

Skip the body for trivial changes (typos, one-line patches). For `feat`,
`perf`, and non-trivial `fix` commits, add a body after a blank line that
explains:

- **Why** — the design decisions and trade-offs, not just what changed
  (e.g. why an independent context, why a cache write lives inside the fn).
- Concurrency/race reasoning when relevant.
- Tests added or updated, and how they are run.

Example:

    feat(proxy): add singleflight to dedup in-flight vision calls

    Cache stampede: N goroutines processing identical images all miss the
    cache and fire N vision calls. Wrap the vision call in
    singleflight.Group.Do(hash, fn) so concurrent callers share one result.
    The group lives on requestHandler so dedup spans across requests.

    go test -race ./... passes.
