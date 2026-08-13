package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
// Claude Code providers (app_type='claude'), and returns their parsed config.
//
// The database is opened read-only. If the file is locked (cc-switch GUI is
// running), it falls back to copying the DB to a temp file. Malformed rows are
// silently skipped.
func ImportFromCcSwitch(dbPath string) ([]CcSwitchProvider, error) {
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
