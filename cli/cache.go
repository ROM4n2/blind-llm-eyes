package cli

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/ROM4n2/blind-llm-eyes/cache"
	"github.com/ROM4n2/blind-llm-eyes/config"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered via database/sql
)

// runCache implements the `cache` subcommand: manage the persistent cache.
// Subcommands: path / stats / list / clear. Each loads config.yaml (override
// with -config) and operates on the SQLite cold layer when type=twotier.
func runCache(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCacheUsage(stderr)
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "path":
		return runCachePath(rest, stdin, stdout, stderr)
	case "stats":
		return runCacheStats(rest, stdin, stdout, stderr)
	case "list":
		return runCacheList(rest, stdin, stdout, stderr)
	case "clear":
		return runCacheClear(rest, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cache: unknown subcommand %q\n", args[0])
		printCacheUsage(stderr)
		return 2
	}
}

// runCachePath prints the configured cache type and db path. For twotier it
// also reports whether the db file currently exists. Always exits 0 on
// success (informational command).
func runCachePath(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache path", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}

	dbPath := resolveDBPath(cfg)
	fmt.Fprintf(stdout, "type: %s\n", cfg.Cache.Type)
	fmt.Fprintf(stdout, "db_path: %s\n", dbPath)
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stdout, "note: type is not twotier; no persistent store")
		return 0
	}
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stdout, "db_exists: false (%v)\n", err)
	} else {
		fmt.Fprintln(stdout, "db_exists: true")
	}
	return 0
}

// runCacheStats prints summary statistics from the SQLite cold layer:
// entry count, total bytes, oldest/newest access timestamps, db file size,
// and the journal mode. Exits 1 if cache is LRU-only (no persistent store).
//
// Drift observation: memory_count (the in-memory atomic counter, initialized
// from COUNT(*) at OpenSQLite time) is displayed alongside actual_count (a
// fresh SELECT COUNT(*)). A >5% divergence is logged as a WARN to stderr,
// indicating either concurrent writes by a running proxy or counter drift
// from failed evictions.
func runCacheStats(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}

	dbPath := resolveDBPath(cfg)
	// Open via cache.OpenSQLite (not openCacheDB) so the in-memory counter
	// is initialized via initCount(), giving us a memory_count baseline.
	s, err := cache.OpenSQLite(dbPath, 0, 0, slog.Default())
	if err != nil {
		fmt.Fprintf(stderr, "open db: %v\n", err)
		return 1
	}
	defer s.Close()
	db := s.DB()

	memCount := s.MemoryCount()
	actualCount, err := s.ActualCount()
	if err != nil {
		fmt.Fprintf(stderr, "query actual count: %v\n", err)
		return 1
	}

	var total int64
	_ = db.QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM cache").Scan(&total)
	var oldest, newest sql.NullInt64
	_ = db.QueryRow("SELECT MIN(last_accessed), MAX(last_accessed) FROM cache").Scan(&oldest, &newest)

	fmt.Fprintf(stdout, "memory_count: %d\n", memCount)
	fmt.Fprintf(stdout, "actual_count: %d\n", actualCount)
	fmt.Fprintf(stdout, "entries: %d\n", actualCount) // backward-compat alias
	fmt.Fprintf(stdout, "total_bytes: %d\n", total)
	if oldest.Valid {
		fmt.Fprintf(stdout, "oldest_access_ms: %d\n", oldest.Int64)
	}
	if newest.Valid {
		fmt.Fprintf(stdout, "newest_access_ms: %d\n", newest.Int64)
	}
	if fi, err := os.Stat(dbPath); err == nil {
		fmt.Fprintf(stdout, "db_file_bytes: %d\n", fi.Size())
	}
	var journalMode string
	_ = db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	fmt.Fprintf(stdout, "journal_mode: %s\n", journalMode)

	// Drift WARN: >5% divergence between in-memory counter and actual DB rows.
	// Causes: concurrent writes by a running proxy (benign), failed evict
	// DELETE not decrementing counter (bug), or external DB modification.
	if actualCount > 0 {
		diff := memCount - actualCount
		if diff < 0 {
			diff = -diff
		}
		if float64(diff)/float64(actualCount) > 0.05 {
			fmt.Fprintf(stderr, "WARN: memory_count (%d) drifts from actual_count (%d) by >5%%\n", memCount, actualCount)
		}
	}
	return 0
}

// runCacheList prints cache entries ordered by most-recently-accessed first.
// Each line shows a 12-char hash prefix, a 60-char description preview, and
// the last_accessed timestamp. The -limit flag caps the number of entries
// (default 20). Exits 1 if cache is LRU-only.
func runCacheList(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	limit := fs.Int("limit", 20, "max entries to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}

	dbPath := resolveDBPath(cfg)
	db, err := openCacheDB(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open db: %v\n", err)
		return 1
	}
	defer db.Close()

	rows, err := db.Query("SELECT hash, description, last_accessed FROM cache ORDER BY last_accessed DESC LIMIT ?", *limit)
	if err != nil {
		fmt.Fprintf(stderr, "query: %v\n", err)
		return 1
	}
	defer rows.Close()

	for rows.Next() {
		var hash, desc string
		var la int64
		if err := rows.Scan(&hash, &desc, &la); err != nil {
			continue
		}
		if len(desc) > 60 {
			desc = desc[:60] + "…"
		}
		if len(hash) > 12 {
			hash = hash[:12]
		}
		fmt.Fprintf(stdout, "%s  %s  (access_ms=%d)\n", hash, desc, la)
	}
	return 0
}

// runCacheClear deletes all entries from the SQLite cold layer. Requires
// confirmation unless -yes is passed. Exits 1 if cache is LRU-only, 2 if
// the user cancels the confirmation prompt.
func runCacheClear(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "path to config file")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Cache.Type != "twotier" {
		fmt.Fprintln(stderr, "cache is LRU-only, no persistent store")
		return 1
	}

	if !*yes {
		fmt.Fprint(stdout, "Delete ALL cache entries? [y/N]: ")
		reader := bufio.NewReader(stdin)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "y" && !strings.EqualFold(line, "yes") {
			fmt.Fprintln(stdout, "cancelled")
			return 2
		}
	}

	dbPath := resolveDBPath(cfg)
	db, err := openCacheDB(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open db: %v\n", err)
		return 1
	}
	defer db.Close()

	res, err := db.Exec("DELETE FROM cache")
	if err != nil {
		fmt.Fprintf(stderr, "delete: %v\n", err)
		return 1
	}
	n, _ := res.RowsAffected()
	fmt.Fprintf(stdout, "deleted: %d\n", n)
	return 0
}

// resolveDBPath returns the configured db_path or the default "./cache.db".
func resolveDBPath(cfg *config.Config) string {
	if cfg.Cache.DBPath != "" {
		return cfg.Cache.DBPath
	}
	return "./cache.db"
}

// openCacheDB opens the SQLite cache database with a busy timeout so the
// CLI can inspect/clear the cache even while the proxy is running. The
// WAL journal_mode is persistent on the file (set by OpenSQLite at proxy
// startup); we don't change it here.
func openCacheDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("busy_timeout: %w", err)
	}
	return db, nil
}

func printCacheUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: blind-llm-eyes cache <subcommand> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  path    Show the cache database path and type")
	fmt.Fprintln(w, "  stats   Show cache statistics (entries, size, oldest/newest access)")
	fmt.Fprintln(w, "  list    List cache entries (hash prefix + description preview)")
	fmt.Fprintln(w, "  clear   Delete all cache entries")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -config string   path to config file (default \"config.yaml\")")
	fmt.Fprintln(w, "  -limit int       max entries to print (list only, default 20)")
	fmt.Fprintln(w, "  -yes             skip confirmation prompt (clear only)")
}
