package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ROM4n2/blind-llm-eyes/modelutil"
	_ "modernc.org/sqlite"
)

// createTestDB creates a cc-switch-compatible SQLite database at path with the
// given provider rows and returns the DSN for read-only access.
func createTestDB(t *testing.T, path string, providers []ccSwitchTestRow) {
	t.Helper()
	// Remove any existing file
	os.Remove(path)

	db, err := openSqlite(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	schema := `CREATE TABLE IF NOT EXISTS providers (
		id          TEXT    NOT NULL,
		app_type    TEXT    NOT NULL,
		name        TEXT    NOT NULL,
		settings_config TEXT NOT NULL,
		website_url TEXT,
		category    TEXT,
		notes       TEXT,
		icon        TEXT,
		meta        TEXT    NOT NULL DEFAULT '{}',
		is_current  BOOLEAN NOT NULL DEFAULT 0,
		PRIMARY KEY (id, app_type)
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create table: %v", err)
	}

	for _, r := range providers {
		if _, err := db.Exec(
			`INSERT INTO providers (id, app_type, name, settings_config) VALUES (?, ?, ?, ?)`,
			r.id, r.appType, r.name, r.settingsConfig,
		); err != nil {
			t.Fatalf("insert provider %q: %v", r.name, err)
		}
	}
}

type ccSwitchTestRow struct {
	id             string
	appType        string
	name           string
	settingsConfig string
}

// makeClaudeSettings builds a settings_config JSON with the given env values.
func makeClaudeSettings(baseURL, apiKey, model string) string {
	env := map[string]string{}
	if baseURL != "" {
		env["ANTHROPIC_BASE_URL"] = baseURL
	}
	if apiKey != "" {
		env["ANTHROPIC_API_KEY"] = apiKey
	}
	if model != "" {
		env["ANTHROPIC_MODEL"] = model
	}
	m := map[string]any{"env": env}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestImportFromCcSwitch_ExtractsProviders(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-deepseek-key", "deepseek-chat[1m]"),
		},
		{
			id:             "mimo-1",
			appType:        "claude",
			name:           "MiMo",
			settingsConfig: makeClaudeSettings("https://api.xiaomimimo.com/anthropic", "sk-mimo-key", "mimo-v2.5[1M]"),
		},
		// Non-claude provider should be ignored
		{
			id:             "codex-1",
			appType:        "codex",
			name:           "OpenAI",
			settingsConfig: `{"env":{"OPENAI_API_KEY":"sk-openai"}}`,
		},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}

	if len(providers) != 2 {
		t.Fatalf("expected 2 claude providers, got %d", len(providers))
	}

	// Check DeepSeek
	ds := providers[0]
	if ds.Name != "DeepSeek" {
		t.Errorf("provider[0] name: got %q, want DeepSeek", ds.Name)
	}
	if ds.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Errorf("provider[0] baseURL: got %q", ds.BaseURL)
	}
	if ds.APIKey != "sk-deepseek-key" {
		t.Errorf("provider[0] apiKey: got %q", ds.APIKey)
	}
	if ds.Model != "deepseek-chat" {
		t.Errorf("provider[0] model: got %q, want deepseek-chat (sanitized from deepseek-chat[1m])", ds.Model)
	}

	// Check MiMo
	mm := providers[1]
	if mm.Name != "MiMo" {
		t.Errorf("provider[1] name: got %q, want MiMo", mm.Name)
	}
	if mm.Model != "mimo-v2.5" {
		t.Errorf("provider[1] model: got %q, want mimo-v2.5 (sanitized from mimo-v2.5[1M])", mm.Model)
	}
}

func TestImportFromCcSwitch_AuthToken(t *testing.T) {
	// Some providers use ANTHROPIC_AUTH_TOKEN instead of ANTHROPIC_API_KEY
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	env := map[string]string{
		"ANTHROPIC_BASE_URL":   "https://api.example.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-auth-token-key",
	}
	settings, _ := json.Marshal(map[string]any{"env": env})

	rows := []ccSwitchTestRow{
		{id: "p1", appType: "claude", name: "TestProvider", settingsConfig: string(settings)},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].APIKey != "sk-auth-token-key" {
		t.Errorf("apiKey: got %q, want sk-auth-token-key", providers[0].APIKey)
	}
}

func TestImportFromCcSwitch_EmptyTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")
	createTestDB(t, dbPath, nil)

	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers, got %d", len(providers))
	}
}

func TestImportFromCcSwitch_NoModel(t *testing.T) {
	// Provider without ANTHROPIC_MODEL — model should be empty
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "p1",
			appType:        "claude",
			name:           "NoModel",
			settingsConfig: makeClaudeSettings("https://api.example.com", "sk-key", ""),
		},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].Model != "" {
		t.Errorf("model: got %q, want empty", providers[0].Model)
	}
}

func TestImportFromCcSwitch_FileNotExist(t *testing.T) {
	_, err := ImportFromCcSwitch("/nonexistent/path/cc-switch.db")
	if err == nil {
		t.Fatal("expected error for nonexistent db file")
	}
}

func TestImportFromCcSwitch_MalformedSettingsConfig(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{id: "bad", appType: "claude", name: "Bad", settingsConfig: "not valid json{"},
	}
	createTestDB(t, dbPath, rows)

	// Should not crash — malformed row is skipped
	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("should tolerate malformed settings_config: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers (malformed skipped), got %d", len(providers))
	}
}

func TestImportFromCcSwitch_MissingEnv(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	// settings_config without env key
	rows := []ccSwitchTestRow{
		{id: "p1", appType: "claude", name: "NoEnv", settingsConfig: `{"theme":"dark"}`},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath)
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers (no env), got %d", len(providers))
	}
}

// TestSanitizeModel_Integration verifies that modelutil.SanitizeModel is
// applied to models from cc-switch.
func TestSanitizeModel_Integration(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"deepseek-chat[1m]", "deepseek-chat"},
		{"mimo-v2.5[1M]", "mimo-v2.5"},
		{"claude-sonnet-4", "claude-sonnet-4"},
	}
	for _, c := range cases {
		got := modelutil.SanitizeModel(c.in)
		if got != c.want {
			t.Errorf("SanitizeModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
