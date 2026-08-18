package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ROM4n2/blind-llm-eyes/modelutil"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

// CcSwitchProvider represents a single Claude Code provider extracted from the
// cc-switch SQLite database.
type CcSwitchProvider struct {
	Name    string // provider display name (from the "name" column)
	BaseURL string // env.ANTHROPIC_BASE_URL
	APIKey  string // env.ANTHROPIC_API_KEY or env.ANTHROPIC_AUTH_TOKEN
	Model   string // env.ANTHROPIC_MODEL, sanitized via modelutil.SanitizeModel
}

// ImportFromCcSwitch opens the cc-switch SQLite database at dbPath, queries all
// Claude Code providers (app_type='claude'), filters out any that point back to
// the proxy itself (self-referential), and returns the parsed config.
//
// The database is opened read-only. If the file is locked (cc-switch GUI is
// running), it falls back to copying the DB to a temp file. Malformed rows are
// silently skipped.
//
// proxyListenAddr is the proxy's own listen address (e.g. "127.0.0.1:8790").
// Providers whose base_url points to this address are filtered out to prevent
// infinite self-forwarding loops. Pass "" to skip filtering.
func ImportFromCcSwitch(dbPath, proxyListenAddr string) ([]CcSwitchProvider, error) {
	db, err := openCcSwitchDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, settings_config FROM providers WHERE app_type = 'claude'`)
	if err != nil {
		return nil, fmt.Errorf("query providers: %w", err)
	}
	defer rows.Close()

	var result []CcSwitchProvider
	for rows.Next() {
		var name, settingsConfig string
		if err := rows.Scan(&name, &settingsConfig); err != nil {
			continue // skip malformed row
		}
		p, ok := parseCcSwitchSettings(name, settingsConfig)
		if !ok {
			continue // skip rows without env or base_url
		}
		if proxyListenAddr != "" && IsSelfReferentialURL(p.BaseURL, proxyListenAddr) {
			continue // skip self-referential providers to prevent infinite loops
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// parseCcSwitchSettings parses a settings_config JSON string and extracts the
// provider info. Returns ok=false if the JSON is malformed or lacks the
// required env.base_url field.
func parseCcSwitchSettings(name, settingsConfig string) (CcSwitchProvider, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsConfig), &top); err != nil {
		return CcSwitchProvider{}, false
	}
	envRaw, ok := top["env"]
	if !ok {
		return CcSwitchProvider{}, false
	}
	var env map[string]string
	if err := json.Unmarshal(envRaw, &env); err != nil {
		return CcSwitchProvider{}, false
	}
	baseURL := env["ANTHROPIC_BASE_URL"]
	if baseURL == "" {
		return CcSwitchProvider{}, false
	}
	apiKey := env["ANTHROPIC_API_KEY"]
	if apiKey == "" {
		apiKey = env["ANTHROPIC_AUTH_TOKEN"]
	}
	model := modelutil.SanitizeModel(env["ANTHROPIC_MODEL"])
	return CcSwitchProvider{
		Name:    name,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
	}, true
}

// openCcSwitchDB opens the cc-switch SQLite database read-only. If the file is
// locked by the GUI, it copies the DB to a temp file and opens that instead.
func openCcSwitchDB(dbPath string) (*sql.DB, error) {
	// Try read-only first
	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cc-switch db: %w", err)
	}
	// Test the connection
	if err := db.Ping(); err != nil {
		db.Close()
		// Fallback: copy to temp file and open
		tmpDB, cerr := copyAndOpenDB(dbPath)
		if cerr != nil {
			return nil, fmt.Errorf("cc-switch db locked and copy fallback failed: %w (original: %v)", cerr, err)
		}
		return tmpDB, nil
	}
	return db, nil
}

// copyAndOpenDB copies the DB file to a temp location and opens it. This is
// the fallback when the GUI has an exclusive lock on the original file.
func copyAndOpenDB(dbPath string) (*sql.DB, error) {
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("read db for copy: %w", err)
	}
	tmpDir := os.TempDir()
	tmpPath := filepath.Join(tmpDir, "blind-llm-eyes-ccswitch-copy.db")
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return nil, fmt.Errorf("write temp db: %w", err)
	}
	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open temp db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping temp db: %w", err)
	}
	return db, nil
}

// openSqlite is a test helper that opens a SQLite database for writing (used
// by test setup to create the schema and insert rows).
func openSqlite(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path)
}

// defaultCcSwitchDBPath returns ~/.cc-switch/cc-switch.db (cross-platform).
func defaultCcSwitchDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home dir: %w", err)
	}
	return filepath.Join(home, ".cc-switch", "cc-switch.db"), nil
}

// IsSelfReferentialURL checks if urlStr points to the proxy's own listen
// address. This prevents infinite self-forwarding loops where a misconfigured
// upstream.base_url points back to the proxy itself.
//
// Comparison is done by extracting host:port from both URLs and comparing
// them after normalization (localhost ↔ 127.0.0.1 ↔ 0.0.0.0). Ports are
// compared as integers so that leading-zero variants (e.g. "08790") don't
// bypass the check — Go's net/http parses "08790" as 8790, so a string
// comparison alone would miss the loop. If the URL cannot be parsed or lacks
// a port, it returns false (safe default).
func IsSelfReferentialURL(urlStr, proxyListenAddr string) bool {
	if urlStr == "" || proxyListenAddr == "" {
		return false
	}
	parsed, err := url.Parse(urlStr)
	if err != nil || parsed.Host == "" {
		return false
	}
	proxyHost, proxyPort, err := net.SplitHostPort(proxyListenAddr)
	if err != nil {
		return false
	}
	urlHost, urlPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		urlPort = portFromScheme(parsed.Scheme)
		urlHost = parsed.Host
	}
	return normalizeHost(urlHost) == normalizeHost(proxyHost) && portsEqual(urlPort, proxyPort)
}

// normalizeHost normalizes host aliases: localhost, 127.0.0.1, 0.0.0.0, ::1
// are all treated as equivalent for self-reference detection. A trailing dot
// (the DNS fully-qualified-name marker, e.g. "127.0.0.1.") is stripped so that
// "127.0.0.1." is recognized as the loopback address — Go's resolver treats
// "127.0.0.1." as 127.0.0.1, so without stripping the check would miss the loop.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimRight(h, ".")
	switch h {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1", "::":
		return "127.0.0.1"
	}
	return h
}

// portsEqual compares two port strings. Numeric ports are parsed and compared
// as integers so that leading-zero forms ("08790") match their canonical form
// ("8790"). If either port is non-numeric, it falls back to exact string
// comparison (so "abc" never equals "8790", preserving the safe-default
// behavior for malformed ports).
func portsEqual(a, b string) bool {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return an == bn
	}
	return a == b
}

// portFromScheme returns the default port for a URL scheme.
func portFromScheme(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
