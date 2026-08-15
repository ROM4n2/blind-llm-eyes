package cache

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered via database/sql
)

// SQLite is the persistent cold-layer cache backed by modernc.org/sqlite.
// Thread-safe: database/sql connection pool + WAL (reads don't block writes).
type SQLite struct {
	db         *sql.DB
	maxEntries int          // capacity cap; <=0 means effectively unlimited
	ttl        time.Duration // TTL; 0 = no TTL eviction
	log        *slog.Logger
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
	sqlUpsert      = `INSERT INTO cache(hash, description, size_bytes, created_at, last_accessed)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			description   = excluded.description,
			size_bytes    = excluded.size_bytes,
			last_accessed = excluded.last_accessed`
)

// OpenSQLite opens (creating if absent) the SQLite cache DB, applies WAL
// PRAGMAs, runs integrity_check (corruption recovery is added in a later
// task — for now a failed integrity check returns an error), and creates
// the schema. path "" defaults to "./cache.db".
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
		db.Close()
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		db.Close()
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

func (s *SQLite) initSchema() error {
	if _, err := s.db.Exec(sqlCreateTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := s.db.Exec(sqlCreateIndex); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) Get(key string) (string, bool) {
	var desc string
	err := s.db.QueryRow(sqlGet, key).Scan(&desc)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		s.log.Warn("sqlite get", "err", err, "key", key)
		return "", false
	}
	// Touch access time (best-effort; failure does not invalidate the hit).
	if _, err := s.db.Exec(sqlTouchAccess, nowMillis(), key); err != nil {
		s.log.Warn("sqlite touch access", "err", err, "key", key)
	}
	return desc, true
}

func (s *SQLite) Put(key, value string) {
	now := nowMillis()
	if _, err := s.db.Exec(sqlUpsert, key, value, len(value), now, now); err != nil {
		s.log.Warn("sqlite put", "err", err, "key", key)
	}
	s.evictIfNeeded()
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// evictIfNeeded is a no-op stub here; real count+TTL eviction is added in the
// next task. Kept so Put compiles without conditional logic.
func (s *SQLite) evictIfNeeded() {}
