package cache

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered via database/sql
)

// SQLite is the persistent cold-layer cache backed by modernc.org/sqlite.
// Thread-safe: database/sql connection pool + WAL (reads don't block writes).
type SQLite struct {
	db         *sql.DB
	maxEntries int           // capacity cap; <=0 means effectively unlimited
	ttl        time.Duration // TTL; 0 = no TTL eviction
	log        *slog.Logger
	count      atomic.Int64 // in-memory row counter; avoids per-Put COUNT(*)
	evicting   atomic.Bool  // CAS guard: at most one evict in flight at a time
	Recorder   TierRecorder // optional; nil = no tier metrics (cold events)
}

const (
	sqlCreateTable = `CREATE TABLE IF NOT EXISTS cache (
		hash           TEXT PRIMARY KEY,
		description    TEXT NOT NULL,
		size_bytes     INTEGER NOT NULL,
		created_at     INTEGER NOT NULL,
		last_accessed  INTEGER NOT NULL
	)`
	sqlCreateIndex = `CREATE INDEX IF NOT EXISTS idx_cache_last_accessed ON cache(last_accessed)`
	sqlGet         = `SELECT description FROM cache WHERE hash = ?`
	sqlTouchAccess = `UPDATE cache SET last_accessed = ? WHERE hash = ?`
	// sqlInsertIgnore + sqlUpdate replace the original ON CONFLICT UPSERT so
	// the in-memory counter can distinguish INSERT (RowsAffected=1) from
	// UPDATE (RowsAffected=0 on the INSERT OR IGNORE step).
	sqlInsertIgnore = `INSERT OR IGNORE INTO cache(hash, description, size_bytes, created_at, last_accessed)
		VALUES(?, ?, ?, ?, ?)`
	sqlUpdate = `UPDATE cache
		SET description = ?, size_bytes = ?, created_at = ?, last_accessed = ?
		WHERE hash = ?`
	sqlCount    = `SELECT COUNT(*) FROM cache`
	sqlEvictLRU = `DELETE FROM cache WHERE hash IN (
		SELECT hash FROM cache ORDER BY last_accessed ASC LIMIT ?)`
	sqlEvictTTL = `DELETE FROM cache WHERE created_at < ?`

	sqlIntegrityCheck = `PRAGMA integrity_check`
)

// OpenSQLite opens (creating if absent) the SQLite cache DB, applies WAL
// PRAGMAs, and runs integrity_check. If applyPragmas or integrity_check
// detects corruption (or errors), the DB + wal + shm files are deleted and
// the handle is reopened — a cold start that loses descriptions but never
// blocks the service. Finally the schema is (re)created. path "" defaults
// to "./cache.db".
func OpenSQLite(path string, maxEntries int, ttl time.Duration, logger *slog.Logger) (*SQLite, error) {
	if path == "" {
		path = "./cache.db"
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)

	s := &SQLite{db: db, maxEntries: maxEntries, ttl: ttl, log: logger}
	if err := s.applyPragmas(); err != nil {
		// modernc errors on PRAGMA journal_mode=WAL with "file is not a
		// database" when the file is corrupt/garbage — this happens before
		// integrity_check can run. Treat any applyPragmas failure as
		// corruption and rebuild so the service never fails to start.
		s.log.Warn("sqlite applyPragmas failed, attempting corruption recovery", "err", err)
		if err := s.rebuildDB(path); err != nil {
			s.db.Close()
			return nil, err
		}
	}
	if err := s.applyCorruptionRecovery(path); err != nil {
		s.db.Close()
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		s.db.Close()
		return nil, err
	}
	if err := s.initCount(); err != nil {
		s.db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) applyPragmas() error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// applyCorruptionRecovery runs PRAGMA integrity_check. If the result is not
// "ok" (or the check itself errors), the DB files are deleted and the handle
// is reopened — a cold start that loses descriptions but never blocks the
// service. After rebuild, initSchema recreates the table.
func (s *SQLite) applyCorruptionRecovery(path string) error {
	var result string
	if err := s.db.QueryRow(sqlIntegrityCheck).Scan(&result); err != nil {
		s.log.Warn("sqlite integrity_check failed, rebuilding", "err", err)
		return s.rebuildDB(path)
	}
	if result != "ok" {
		s.log.Warn("sqlite integrity_check not ok, rebuilding", "result", result)
		return s.rebuildDB(path)
	}
	return nil
}

// rebuildDB closes the current handle, removes the db + wal + shm files, and
// reopens a fresh handle with the WAL pragmas applied. The schema is recreated
// by the caller's initSchema step that follows OpenSQLite's recovery call.
func (s *SQLite) rebuildDB(path string) error {
	s.db.Close()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(f)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("reopen sqlite after rebuild: %w", err)
	}
	db.SetMaxOpenConns(8)
	s.db = db
	if err := s.applyPragmas(); err != nil {
		return err
	}
	return nil
}

func (s *SQLite) initSchema() error {
	if _, err := s.db.Exec(sqlCreateTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := s.db.Exec(sqlCreateIndex); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	return nil
}

// initCount populates the in-memory row counter with a one-shot COUNT(*).
// Called once at startup (after initSchema); subsequent Put/evict operations
// maintain the counter via atomic adds, avoiding per-Put full table scans.
func (s *SQLite) initCount() error {
	var n int
	if err := s.db.QueryRow(sqlCount).Scan(&n); err != nil {
		return fmt.Errorf("init count: %w", err)
	}
	s.count.Store(int64(n))
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB handle. Used by CLI tooling for ad-hoc
// inspection queries (PRAGMA journal_mode, SUM(size_bytes), MIN/MAX access).
// Callers must not Close the returned handle; use SQLite.Close instead.
func (s *SQLite) DB() *sql.DB { return s.db }

// MemoryCount returns the in-memory atomic row counter. This is maintained
// incrementally on Put/evict and may drift from ActualCount if a DELETE
// fails or an external writer modifies the DB. Use ActualCount to reconcile.
func (s *SQLite) MemoryCount() int64 { return s.count.Load() }

// ActualCount queries the DB for the real row count (SELECT COUNT(*)). This
// is an O(N) full table scan and should only be used for diagnostics, not
// on the hot path. Compare with MemoryCount to observe counter drift.
func (s *SQLite) ActualCount() (int64, error) {
	var n int64
	if err := s.db.QueryRow(sqlCount).Scan(&n); err != nil {
		return 0, fmt.Errorf("actual count: %w", err)
	}
	return n, nil
}

func (s *SQLite) Get(key string) (string, bool) {
	var desc string
	err := s.db.QueryRow(sqlGet, key).Scan(&desc)
	if err == sql.ErrNoRows {
		if s.Recorder != nil {
			s.Recorder.OnLookup("cold", "miss")
		}
		return "", false
	}
	if err != nil {
		s.log.Warn("sqlite get", "key", key, "err", err)
		// Errors (corrupt, locked) look like a miss to callers. Don't report to
		// recorder so cold-miss ratios aren't polluted by infrastructure
		// faults — operators see those via logs/errors_total.
		return "", false
	}
	if s.Recorder != nil {
		s.Recorder.OnLookup("cold", "hit")
	}
	// Touch access time (best-effort; failure does not invalidate the hit).
	if _, err := s.db.Exec(sqlTouchAccess, nowMillis(), key); err != nil {
		s.log.Warn("sqlite touch access", "err", err, "key", key)
	}
	return desc, true
}

func (s *SQLite) Put(key, value string) {
	now := nowMillis()
	// INSERT OR IGNORE: RowsAffected=1 on new row, 0 on existing. This lets
	// the in-memory counter distinguish inserts from updates without a
	// separate COUNT(*) query on every Put.
	res, err := s.db.Exec(sqlInsertIgnore, key, value, len(value), now, now)
	if err != nil {
		s.log.Warn("sqlite put", "err", err, "key", key)
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// Key exists; update fields without touching the counter.
		if _, err := s.db.Exec(sqlUpdate, value, len(value), now, now, key); err != nil {
			s.log.Warn("sqlite update", "err", err, "key", key)
			return
		}
	} else {
		s.count.Add(1) // new row inserted
	}
	s.evictIfNeeded()
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// evictIfNeeded enforces the SQLite-layer caps using the in-memory counter
// (O(1)) instead of a per-Put COUNT(*) (O(N) full table scan). Count-based
// eviction trims to 90% of maxEntries when over (batch delete to amortize).
// TTL eviction drops entries older than ttl. Both are best-effort; failures
// log a WARN. On successful DELETE the counter is decremented by the actual
// number of rows removed (rowsAffected) to prevent drift.
//
// CAS guard: a CompareAndSwap on s.evicting ensures at most one evict runs
// at a time. Without this, bursty concurrent Puts (e.g., after vision
// provider recovery) each cross maxEntries simultaneously and each run
// DELETE LIMIT del, clearing the cache far below the cap. Losers return
// early; their count.Add(1) is already accounted for, and the next Put
// will retry the CAS and converge. Trade-off: count may transiently exceed
// maxEntries during the burst window.
func (s *SQLite) evictIfNeeded() {
	if !s.evicting.CompareAndSwap(false, true) {
		return
	}
	defer s.evicting.Store(false)

	n := s.count.Load()
	if s.maxEntries > 0 && n > int64(s.maxEntries) {
		del := n - int64(s.maxEntries)*9/10
		if del < 1 {
			del = 1
		}
		res, err := s.db.Exec(sqlEvictLRU, del)
		if err != nil {
			s.log.Warn("sqlite evict lru", "err", err)
		} else if deleted, _ := res.RowsAffected(); deleted > 0 {
			s.count.Add(-deleted)
		}
	}
	if s.ttl > 0 {
		cutoff := nowMillis() - s.ttl.Milliseconds()
		res, err := s.db.Exec(sqlEvictTTL, cutoff)
		if err != nil {
			s.log.Warn("sqlite evict ttl", "err", err)
		} else if deleted, _ := res.RowsAffected(); deleted > 0 {
			s.count.Add(-deleted)
		}
	}
}

// PragmaQuickCheck runs `PRAGMA quick_check` and returns nil iff the result
// is exactly "ok". Used by doctor D1 to detect low-level DB corruption
// (faster than the full integrity_check).
func (s *SQLite) PragmaQuickCheck() error {
	var res string
	if err := s.db.QueryRow("PRAGMA quick_check").Scan(&res); err != nil {
		return fmt.Errorf("quick_check query: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("quick_check reported: %s", res)
	}
	return nil
}

// WriteProbe inserts a uniquely-keyed probe row with the given hash and a
// non-empty description, then immediately deletes it (cleanup). Returns nil
// only when both write + delete succeed, proving round-trip write capability.
// Used by doctor D1 as the "can actually persist data" smoke-test.
func (s *SQLite) WriteProbe(hash string) error {
	desc := "doctor-write-probe"
	size := int64(len(desc))
	now := nowMillis()
	_, err := s.db.Exec(sqlInsertIgnore, hash, desc, size, now, now)
	if err != nil {
		return fmt.Errorf("probe insert: %w", err)
	}
	// Clean up — keep the cache table tidy. RowsAffected check intentionally
	// skipped: if insert was a no-op (INSERT OR IGNORE) we still want to
	// delete any stray leftover probe row from a prior interrupted run.
	if _, err := s.db.Exec(`DELETE FROM cache WHERE hash = ?`, hash); err != nil {
		return fmt.Errorf("probe cleanup: %w", err)
	}
	return nil
}
