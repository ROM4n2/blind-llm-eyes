package cli

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // register sqlite driver for database/sql
)

// writeCacheConfig writes a minimal valid config.yaml to cfgPath. If twotier
// is true, cache.type=twotier and cache.db_path=dbPath are included.
func writeCacheConfig(t *testing.T, cfgPath string, twotier bool, dbPath string) {
	t.Helper()
	content := "upstream: {base_url: http://x}\nvision: {base_url: http://v, api_key: k, model: m}\n"
	if twotier {
		// Use YAML single quotes: backslashes are literal (no \U escape).
		content += "cache: {type: twotier, db_path: '" + dbPath + "'}\n"
	}
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// prepCacheDB creates a fresh SQLite db at dbPath with the cache schema and
// inserts the given (hash, description) pairs. size_bytes=len(desc),
// created_at and last_accessed are set to incrementing values starting at 1.
func prepCacheDB(t *testing.T, dbPath string, entries ...[2]string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE cache(
		hash          TEXT PRIMARY KEY,
		description   TEXT NOT NULL,
		size_bytes    INTEGER NOT NULL,
		created_at    INTEGER NOT NULL,
		last_accessed INTEGER NOT NULL)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i, e := range entries {
		_, err = db.Exec("INSERT INTO cache VALUES(?, ?, ?, ?, ?)",
			e[0], e[1], len(e[1]), int64(i+1), int64(i+1))
		if err != nil {
			t.Fatalf("insert [%d]: %v", i, err)
		}
	}
}

// ──────────────────────────── cache path ────────────────────────────

func TestRunCache_Path_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCacheConfig(t, cfgPath, false, "")
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "type: lru") {
		t.Errorf("out: %s", out.String())
	}
	if !strings.Contains(out.String(), "no persistent store") {
		t.Errorf("out: %s", out.String())
	}
}

func TestRunCache_Path_TwoTierNoDB(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "type: twotier") {
		t.Errorf("out: %s", out.String())
	}
	if !strings.Contains(out.String(), "db_exists: false") {
		t.Errorf("out: %s", out.String())
	}
}

func TestRunCache_Path_TwoTierWithDB(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath, [2]string{"h1", "v1"}) // create the db file
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "db_exists: true") {
		t.Errorf("out: %s", out.String())
	}
}

func TestRunCache_Path_ConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"path"}, nil, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(errB.String(), "load config") {
		t.Errorf("err: %s", errB.String())
	}
}

// ──────────────────────────── cache stats ────────────────────────────

func TestRunCache_Stats_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCacheConfig(t, cfgPath, false, "")
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"stats"}, nil, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(errB.String(), "LRU-only") {
		t.Errorf("err: %s", errB.String())
	}
}

func TestRunCache_Stats_TwoTier(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath,
		[2]string{"h1", "v1"},
		[2]string{"h2", "v2"},
	)
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"stats"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "entries: 2") {
		t.Errorf("out: %s", out.String())
	}
	if !strings.Contains(out.String(), "total_bytes: 4") {
		t.Errorf("out: %s", out.String())
	}
	// Drift observation fields: memory_count (in-memory counter initialized
	// from COUNT(*) at OpenSQLite time) and actual_count (fresh COUNT(*)).
	// With no concurrent writer they must agree.
	if !strings.Contains(out.String(), "memory_count: 2") {
		t.Errorf("want memory_count: 2, out: %s", out.String())
	}
	if !strings.Contains(out.String(), "actual_count: 2") {
		t.Errorf("want actual_count: 2, out: %s", out.String())
	}
	// No drift WARN expected when counts agree.
	if strings.Contains(errB.String(), "WARN") {
		t.Errorf("unexpected drift WARN: %s", errB.String())
	}
}

func TestRunCache_Stats_TwoTierEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	// Create an empty db (schema only, no rows).
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE cache(
		hash          TEXT PRIMARY KEY,
		description   TEXT NOT NULL,
		size_bytes    INTEGER NOT NULL,
		created_at    INTEGER NOT NULL,
		last_accessed INTEGER NOT NULL)`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"stats"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "entries: 0") {
		t.Errorf("out: %s", out.String())
	}
	if !strings.Contains(out.String(), "total_bytes: 0") {
		t.Errorf("out: %s", out.String())
	}
}

// ──────────────────────────── cache list ────────────────────────────

func TestRunCache_List(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath, [2]string{"abcdef0123456789", "a cat on a mat"})
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"list", "-limit", "5"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	// hash truncated to 12 chars: "abcdef012345"
	if !strings.Contains(out.String(), "abcdef0123") {
		t.Errorf("want hash prefix: %s", out.String())
	}
	if !strings.Contains(out.String(), "a cat on a mat") {
		t.Errorf("want desc: %s", out.String())
	}
}

func TestRunCache_List_DescTruncation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	longDesc := strings.Repeat("x", 80) // > 60 chars
	prepCacheDB(t, dbPath, [2]string{"hash1234567890", longDesc})
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"list"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	// Description should be truncated to 60 chars + ellipsis.
	if !strings.Contains(out.String(), "…") {
		t.Errorf("want truncated desc with ellipsis: %s", out.String())
	}
	if strings.Contains(out.String(), strings.Repeat("x", 61)) {
		t.Errorf("desc not truncated: %s", out.String())
	}
}

func TestRunCache_List_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCacheConfig(t, cfgPath, false, "")
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"list"}, nil, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(errB.String(), "LRU-only") {
		t.Errorf("err: %s", errB.String())
	}
}

// ──────────────────────────── cache clear ────────────────────────────

func TestRunCache_Clear_WithYes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath,
		[2]string{"h1", "v1"},
		[2]string{"h2", "v2"},
	)
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"clear", "-yes"}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "deleted: 2") {
		t.Errorf("out: %s", out.String())
	}

	// Verify the db is now empty.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM cache").Scan(&n)
	if n != 0 {
		t.Errorf("after clear, db still has %d entries", n)
	}
}

func TestRunCache_Clear_Cancel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath, [2]string{"h1", "v1"})
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"clear"}, strings.NewReader("n\n"), &out, &errB)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Errorf("out: %s", out.String())
	}

	// Verify the db still has the entry.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM cache").Scan(&n)
	if n != 1 {
		t.Errorf("after cancel, db should still have 1 entry, got %d", n)
	}
}

func TestRunCache_Clear_ConfirmYes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "cache.db")
	writeCacheConfig(t, cfgPath, true, dbPath)
	prepCacheDB(t, dbPath, [2]string{"h1", "v1"})
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"clear"}, strings.NewReader("y\n"), &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "deleted: 1") {
		t.Errorf("out: %s", out.String())
	}
}

func TestRunCache_Clear_LRUOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeCacheConfig(t, cfgPath, false, "")
	t.Chdir(dir)

	var out, errB bytes.Buffer
	code := runCache([]string{"clear", "-yes"}, nil, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
	if !strings.Contains(errB.String(), "LRU-only") {
		t.Errorf("err: %s", errB.String())
	}
}

// ──────────────────────────── dispatch ────────────────────────────

func TestRunCache_NoArgs_PrintsUsage(t *testing.T) {
	var out, errB bytes.Buffer
	code := runCache(nil, nil, &out, &errB)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
	if !strings.Contains(errB.String(), "Subcommands") {
		t.Errorf("err: %s", errB.String())
	}
}

func TestRunCache_UnknownSubcommand(t *testing.T) {
	var out, errB bytes.Buffer
	code := runCache([]string{"frob"}, nil, &out, &errB)
	if code != 2 {
		t.Fatalf("want 2, got %d", code)
	}
	if !strings.Contains(errB.String(), "unknown subcommand") {
		t.Errorf("err: %s", errB.String())
	}
}

func TestRunCache_ConfigFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "my-config.yaml")
	writeCacheConfig(t, cfgPath, false, "")
	// Don't chdir; use -config to point at the absolute path.

	var out, errB bytes.Buffer
	code := runCache([]string{"path", "-config", cfgPath}, nil, &out, &errB)
	if code != 0 {
		t.Fatalf("code %d %s", code, errB.String())
	}
	if !strings.Contains(out.String(), "type: lru") {
		t.Errorf("out: %s", out.String())
	}
}
