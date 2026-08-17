# SQLite In-Memory Counter Implementation Plan

> **For agentic workers:** This is a single-task perf optimization. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace per-`Put` `SELECT COUNT(*)` (O(N) full table scan) with an
in-memory `atomic.Int64` counter, dropping the hot-path eviction check from
O(N) to O(1).

**Architecture:** Add `count atomic.Int64` to `SQLite`. Initialize once at
startup via `COUNT(*)`. On `Put`, distinguish INSERT vs UPDATE by splitting
the UPSERT into `INSERT OR IGNORE` + `UPDATE`, using `RowsAffected` to
increment the counter only on new inserts. `evictIfNeeded` reads the atomic
counter and subtracts `RowsAffected` from DELETE results. `rebuildDB` zeroes
the counter to prevent drift after corruption recovery.

**Tech Stack:** Go 1.26, `database/sql`, `modernc.org/sqlite`, `sync/atomic`.

---

## Motivation

`cache/sqlite.go:evictIfNeeded` runs `SELECT COUNT(*) FROM cache` on every
`Put`. SQLite's `COUNT(*)` is an O(N) full table scan (indexes do not help).

| DB rows | Single COUNT cost | At 1000 writes/s |
|---|---|---|
| 1k | ~0.1ms | 100ms/s (10% CPU) |
| 10k | ~1ms | 1s/s (unacceptable) |
| 100k | ~10ms | blocks writer |
| 1M | ~100ms+ | severe blocking |

## Design

### Structure

```go
type SQLite struct {
    db         *sql.DB
    maxEntries int
    ttl        time.Duration
    log        *slog.Logger
    count      atomic.Int64  // in-memory row counter
}
```

### SQL: UPSERT -> INSERT OR IGNORE + UPDATE

The original `sqlUpsert` uses `ON CONFLICT DO UPDATE`. Both INSERT and UPDATE
paths return `RowsAffected=1` in SQLite, so the counter cannot distinguish
them. Split into two statements:

```go
const (
    sqlInsertIgnore = `INSERT OR IGNORE INTO cache(hash, description, size_bytes, created_at, last_accessed)
        VALUES(?, ?, ?, ?, ?)`
    sqlUpdate = `UPDATE cache
        SET description = ?, size_bytes = ?, created_at = ?, last_accessed = ?
        WHERE hash = ?`
)
```

- `INSERT OR IGNORE`: `RowsAffected=1` on new, `0` on existing (clear semantics).
- Existing path runs a separate `UPDATE` (PK index lookup, sub-ms).

### Put

```go
func (s *SQLite) Put(key, value string) {
    now := nowMillis()
    res, err := s.db.Exec(sqlInsertIgnore, key, value, len(value), now, now)
    if err != nil {
        s.log.Warn("sqlite put", "err", err, "key", key)
        return
    }
    if affected, _ := res.RowsAffected(); affected == 0 {
        if _, err := s.db.Exec(sqlUpdate, value, len(value), now, now, key); err != nil {
            s.log.Warn("sqlite update", "err", err, "key", key)
            return
        }
    } else {
        s.count.Add(1)  // new row
    }
    s.evictIfNeeded()
}
```

### evictIfNeeded

```go
func (s *SQLite) evictIfNeeded() {
    n := s.count.Load()
    if s.maxEntries > 0 && n > int64(s.maxEntries) {
        del := n - int64(s.maxEntries)*9/10
        if del < 1 { del = 1 }
        res, err := s.db.Exec(sqlEvictLRU, del)
        if err == nil {
            if deleted, _ := res.RowsAffected(); deleted > 0 {
                s.count.Add(-deleted)
            }
        } else {
            s.log.Warn("sqlite evict lru", "err", err)
        }
    }
    if s.ttl > 0 {
        cutoff := nowMillis() - s.ttl.Milliseconds()
        res, err := s.db.Exec(sqlEvictTTL, cutoff)
        if err == nil {
            if deleted, _ := res.RowsAffected(); deleted > 0 {
                s.count.Add(-deleted)
            }
        } else {
            s.log.Warn("sqlite evict ttl", "err", err)
        }
    }
}
```

### Startup initialization

Call `initCount` after `initSchema` inside `OpenSQLite`:

```go
func (s *SQLite) initCount() error {
    var n int
    if err := s.db.QueryRow(sqlCount).Scan(&n); err != nil {
        return fmt.Errorf("init count: %w", err)
    }
    s.count.Store(int64(n))
    return nil
}
```

One `COUNT(*)` at startup is acceptable; zero during runtime.

### Corruption recovery reset

`rebuildDB` deletes db + wal + shm and reopens. The counter must be zeroed
to prevent drift. Add `s.count.Store(0)` at the end of `rebuildDB` (the
caller's `initSchema` + `initCount` will repopulate on a fresh empty DB).

## Risks & Mitigations

| Risk | Assessment | Mitigation |
|---|---|---|
| Counter drift under concurrent inserts | `atomic.Add` is atomic; cannot undercount | Over-count -> evict a bit more; under-count -> evict a bit less; maxEntries cap semantics preserved |
| Two SQL statements slower than one UPSERT? | `INSERT OR IGNORE` IGNORE path is index-only; `UPDATE` is PK lookup, sub-ms | Net win still dwarfs the saved COUNT |
| Counter stale after corruption recovery | `rebuildDB` zeroes counter explicitly | Unit test covers this path |
| `RowsAffected` unreliable in modernc? | modernc.org/sqlite implements `Result.RowsAffected` standardly; existing `sqlEvictLRU` already relies on it | Covered by existing eviction tests |

## Scope

- Modify: `cache/sqlite.go`, `cache/sqlite_test.go`
- Do NOT touch: `twotier.go`, `lru.go`, `handler.go`, `main.go`, config
- Backward compatible: `Get`/`Put` signatures unchanged, eviction semantics unchanged

## Tasks

### Task 1: Write failing tests

**Files:**
- Modify: `cache/sqlite_test.go`

- [ ] **Step 1: Write the failing tests**

Append three tests using existing `newTestSQLite` / `OpenSQLite + t.TempDir()` helpers:

```go
func TestSQLite_PutUpdatesInMemoryCount(t *testing.T) {
    s := newTestSQLite(t)
    defer s.Close()
    if got := s.count.Load(); got != 0 {
        t.Fatalf("initial count = %d, want 0", got)
    }
    s.Put("h1", "desc1")
    s.Put("h2", "desc2")
    s.Put("h3", "desc3")
    if got := s.count.Load(); got != 3 {
        t.Fatalf("after 3 inserts count = %d, want 3", got)
    }
    s.Put("h1", "desc1-updated")
    if got := s.count.Load(); got != 3 {
        t.Fatalf("after update count = %d, want 3", got)
    }
    var dbN int
    if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
        t.Fatal(err)
    }
    if int64(dbN) != s.count.Load() {
        t.Fatalf("counter %d != db rows %d", s.count.Load(), dbN)
    }
}

func TestSQLite_EvictDecrementsCount(t *testing.T) {
    path := filepath.Join(t.TempDir(), "cache.db")
    s, err := OpenSQLite(path, 5, 0, discardLogger())
    if err != nil {
        t.Fatalf("OpenSQLite: %v", err)
    }
    defer s.Close()
    for i := 0; i < 10; i++ {
        s.Put(fmt.Sprintf("h%d", i), "desc")
    }
    if got := s.count.Load(); got > 5 {
        t.Fatalf("after evict count = %d, want <= 5", got)
    }
    var dbN int
    if err := s.db.QueryRow(sqlCount).Scan(&dbN); err != nil {
        t.Fatal(err)
    }
    if int64(dbN) != s.count.Load() {
        t.Fatalf("counter %d != db rows %d after evict", s.count.Load(), dbN)
    }
}

func TestSQLite_RebuildResetsCount(t *testing.T) {
    path := filepath.Join(t.TempDir(), "cache.db")
    s, err := OpenSQLite(path, 100, 0, discardLogger())
    if err != nil {
        t.Fatalf("OpenSQLite: %v", err)
    }
    defer s.Close()
    s.Put("h1", "desc1")
    s.Put("h2", "desc2")
    if got := s.count.Load(); got != 2 {
        t.Fatalf("pre-rebuild count = %d, want 2", got)
    }
    if err := s.rebuildDB(path); err != nil {
        t.Fatalf("rebuildDB: %v", err)
    }
    if err := s.initSchema(); err != nil {
        t.Fatalf("initSchema: %v", err)
    }
    if err := s.initCount(); err != nil {
        t.Fatalf("initCount: %v", err)
    }
    if got := s.count.Load(); got != 0 {
        t.Fatalf("post-rebuild count = %d, want 0", got)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestSQLite_PutUpdatesInMemoryCount|TestSQLite_EvictDecrementsCount|TestSQLite_RebuildResetsCount' ./cache -v`
Expected: compile failure (`s.count undefined`, `s.initCount undefined`).

### Task 2: Implement in-memory counter

**Files:**
- Modify: `cache/sqlite.go`

- [ ] **Step 3: Apply the implementation changes**

1. Add `sync/atomic` to imports (note: `atomic.Int64` requires Go 1.19+, we're on 1.26).
2. Add `count atomic.Int64` field to `SQLite` struct.
3. Replace `sqlUpsert` const with `sqlInsertIgnore` + `sqlUpdate` consts.
4. Rewrite `Put` to use `INSERT OR IGNORE` + `UPDATE` and bump counter on new insert.
5. Rewrite `evictIfNeeded` to read `s.count.Load()` and subtract DELETE `RowsAffected`.
6. Add `initCount` method.
7. In `OpenSQLite`, call `s.initCount()` after `s.initSchema()`.
8. In `rebuildDB`, after successful reopen + pragma application, call `s.count.Store(0)`.

- [ ] **Step 4: Run all cache tests with -race**

Run: `go test -race -count=1 ./cache/... -v`
Expected: all tests PASS (new + existing).

- [ ] **Step 5: Run go vet**

Run: `go vet ./...`
Expected: no output.

### Task 3: Commit

- [ ] **Step 6: Commit**

```bash
git add cache/sqlite.go cache/sqlite_test.go
git commit -m "$(cat <<'EOF'
perf(cache): avoid per-put count(*) via in-memory counter

evictIfNeeded ran SELECT COUNT(*) on every Put, an O(N) full table
scan. Replace with an atomic.Int64 counter initialized at startup
and updated on INSERT (RowsAffected=1) and evict DELETE. UPDATE
path now uses a separate UPDATE statement (INSERT OR IGNORE +
UPDATE split from the original UPSERT to distinguish new vs
existing). RebuildDB zeroes the counter to prevent drift after
corruption recovery.
EOF
)"
```

## Acceptance Criteria

- [ ] 3 new unit tests pass (Put increments counter / evict decrements / rebuild resets)
- [ ] All existing cache tests pass under `-race`
- [ ] `go vet ./...` clean
- [ ] No changes to files outside `cache/`
