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

	providers, err := ImportFromCcSwitch(dbPath, "")
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

	providers, err := ImportFromCcSwitch(dbPath, "")
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

	providers, err := ImportFromCcSwitch(dbPath, "")
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

	providers, err := ImportFromCcSwitch(dbPath, "")
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
	_, err := ImportFromCcSwitch("/nonexistent/path/cc-switch.db", "")
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
	providers, err := ImportFromCcSwitch(dbPath, "")
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

	providers, err := ImportFromCcSwitch(dbPath, "")
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

// ── Self-reference detection tests ──

func TestIsSelfReferentialURL(t *testing.T) {
	cases := []struct {
		urlStr  string
		listen  string
		selfRef bool
	}{
		// Same host:port → self-referential
		{"http://127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
		{"http://localhost:8790/anthropic", "127.0.0.1:8790", true},
		{"http://0.0.0.0:8790/anthropic", "127.0.0.1:8790", true},
		{"http://[::1]:8790/anthropic", "127.0.0.1:8790", true},

		// Different port → not self-referential
		{"http://127.0.0.1:8080/anthropic", "127.0.0.1:8790", false},
		{"http://127.0.0.1:8790/anthropic", "127.0.0.1:8080", false},

		// Different host → not self-referential
		{"http://api.deepseek.com/anthropic", "127.0.0.1:8790", false},

		// HTTPS on same port → self-referential (same host:port, regardless of scheme)
		{"https://127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},

		// Empty inputs → not self-referential (safe default)
		{"", "127.0.0.1:8790", false},
		{"http://127.0.0.1:8790", "", false},

		// URL without explicit port (uses scheme default)
		{"http://127.0.0.1/anthropic", "127.0.0.1:80", true},
	}
	for _, c := range cases {
		got := IsSelfReferentialURL(c.urlStr, c.listen)
		if got != c.selfRef {
			t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.selfRef)
		}
	}
}

// TestIsSelfReferentialURL_PortBoundaries focuses on port-related edge cases:
// default ports (80/443), port asymmetry between URL and listen addr, and
// scenarios where the URL omits the port entirely.
func TestIsSelfReferentialURL_PortBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		urlStr  string
		listen  string
		selfRef bool
	}{
		// ── Default port inference (URL omits port) ──
		{
			name:    "http default port 80 matches listen :80",
			urlStr:  "http://127.0.0.1/anthropic",
			listen:  "127.0.0.1:80",
			selfRef: true,
		},
		{
			name:    "https default port 443 matches listen :443",
			urlStr:  "https://127.0.0.1/anthropic",
			listen:  "127.0.0.1:443",
			selfRef: true,
		},
		{
			name:    "http default port 80 does NOT match listen :443",
			urlStr:  "http://127.0.0.1/anthropic",
			listen:  "127.0.0.1:443",
			selfRef: false,
		},
		{
			name:    "https default port 443 does NOT match listen :80",
			urlStr:  "https://127.0.0.1/anthropic",
			listen:  "127.0.0.1:80",
			selfRef: false,
		},
		{
			name:    "http default port 80 does NOT match listen :8790",
			urlStr:  "http://127.0.0.1/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: false,
		},

		// ── Port asymmetry: one side has port, other doesn't ──
		{
			name:    "explicit port URL vs default-port listen (http)",
			urlStr:  "http://127.0.0.1:80/anthropic",
			listen:  "127.0.0.1:80",
			selfRef: true,
		},
		{
			name:    "explicit port 8080 vs default port 80 (http)",
			urlStr:  "http://127.0.0.1:8080/anthropic",
			listen:  "127.0.0.1:80",
			selfRef: false,
		},

		// ── Same port, different host aliases (all normalize to 127.0.0.1) ──
		{
			name:    "0.0.0.0:8790 vs 127.0.0.1:8790",
			urlStr:  "http://0.0.0.0:8790/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: true,
		},
		{
			name:    "127.0.0.1:8790 vs 0.0.0.0:8790 (reverse)",
			urlStr:  "http://127.0.0.1:8790/anthropic",
			listen:  "0.0.0.0:8790",
			selfRef: true,
		},
		{
			name:    "localhost:8790 vs 0.0.0.0:8790",
			urlStr:  "http://localhost:8790/anthropic",
			listen:  "0.0.0.0:8790",
			selfRef: true,
		},
		{
			name:    "[::1]:8790 vs 127.0.0.1:8790 (IPv6 vs IPv4)",
			urlStr:  "http://[::1]:8790/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: true,
		},

		// ── IPv6 with port ──
		{
			name:    "[::1]:8790 vs [::1]:8790",
			urlStr:  "http://[::1]:8790/anthropic",
			listen:  "[::1]:8790",
			selfRef: true,
		},
		{
			name:    "[::1]:8790 vs [::1]:8080 (different port)",
			urlStr:  "http://[::1]:8790/anthropic",
			listen:  "[::1]:8080",
			selfRef: false,
		},

		// ── Port edge values ──
		{
			name:    "port 1 vs port 1 (lowest valid port)",
			urlStr:  "http://127.0.0.1:1/anthropic",
			listen:  "127.0.0.1:1",
			selfRef: true,
		},
		{
			name:    "port 65535 vs port 65535 (highest valid port)",
			urlStr:  "http://127.0.0.1:65535/anthropic",
			listen:  "127.0.0.1:65535",
			selfRef: true,
		},
		{
			name:    "port 1 vs port 2 (adjacent ports)",
			urlStr:  "http://127.0.0.1:1/anthropic",
			listen:  "127.0.0.1:2",
			selfRef: false,
		},

		// ── URL with path/query/userinfo (port extraction still works) ──
		{
			name:    "URL with long path, same port",
			urlStr:  "http://127.0.0.1:8790/anthropic/v1/messages",
			listen:  "127.0.0.1:8790",
			selfRef: true,
		},
		{
			name:    "URL with query string, same port",
			urlStr:  "http://127.0.0.1:8790/?foo=bar&baz=qux",
			listen:  "127.0.0.1:8790",
			selfRef: true,
		},
		{
			name:    "URL with userinfo, same port",
			urlStr:  "http://user:pass@127.0.0.1:8790/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: true,
		},

		// ── Invalid/malformed inputs (safe defaults: NOT self-referential) ──
		{
			name:    "URL with non-numeric port",
			urlStr:  "http://127.0.0.1:abc/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: false,
		},
		{
			name:    "listen addr with non-numeric port",
			urlStr:  "http://127.0.0.1:8790/anthropic",
			listen:  "127.0.0.1:abc",
			selfRef: false,
		},
		{
			name:    "listen addr without port (SplitHostPort fails)",
			urlStr:  "http://127.0.0.1:8790/anthropic",
			listen:  "127.0.0.1",
			selfRef: false,
		},
		{
			name:    "URL without scheme (url.Parse may still work)",
			urlStr:  "127.0.0.1:8790/anthropic",
			listen:  "127.0.0.1:8790",
			selfRef: false, // parsed.Host is empty without scheme
		},
		{
			name:    "empty URL string",
			urlStr:  "",
			listen:  "127.0.0.1:8790",
			selfRef: false,
		},
		{
			name:    "empty listen addr",
			urlStr:  "http://127.0.0.1:8790/anthropic",
			listen:  "",
			selfRef: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsSelfReferentialURL(c.urlStr, c.listen)
			if got != c.selfRef {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.selfRef)
			}
		})
	}
}

func TestImportFromCcSwitch_FiltersSelfReferential(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-1",
			appType:        "claude",
			name:           "SelfRef",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-2",
			appType:        "claude",
			name:           "SelfRefLocalhost",
			settingsConfig: makeClaudeSettings("http://localhost:8790/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	// Without filtering (empty listen addr), all 3 should be returned
	providers, err := ImportFromCcSwitch(dbPath, "")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 3 {
		t.Errorf("expected 3 providers without filtering, got %d", len(providers))
	}

	// With filtering, only the non-self-referential provider should remain
	providers, err = ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider after filtering self-referential, got %d", len(providers))
	}
	if providers[0].Name != "DeepSeek" {
		t.Errorf("expected DeepSeek, got %q", providers[0].Name)
	}
}

// TestImportFromCcSwitch_AllSelfReferential_ReturnsEmpty verifies that when
// every provider in the cc-switch DB points back to the proxy's own listen
// address, ImportFromCcSwitch filters them all out and returns an empty list
// (not nil-deref or error). This is the "user accidentally configured all
// providers as self-loops" edge case.
func TestImportFromCcSwitch_AllSelfReferential_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "self-1",
			appType:        "claude",
			name:           "SelfRef1",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-2",
			appType:        "claude",
			name:           "SelfRef2",
			settingsConfig: makeClaudeSettings("http://localhost:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-3",
			appType:        "claude",
			name:           "SelfRef3",
			settingsConfig: makeClaudeSettings("https://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers when all are self-referential, got %d: %+v", len(providers), providers)
	}

	// Without filtering, all 3 should be returned (sanity check).
	providers, err = ImportFromCcSwitch(dbPath, "")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch (no filter): %v", err)
	}
	if len(providers) != 3 {
		t.Errorf("expected 3 providers without filtering, got %d", len(providers))
	}
}

// TestImportFromCcSwitch_NoneSelfReferential_KeepsAll verifies that when no
// provider is self-referential, ImportFromCcSwitch keeps all of them — the
// filter is a no-op and does not accidentally drop valid entries.
func TestImportFromCcSwitch_NoneSelfReferential_KeepsAll(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "glm-1",
			appType:        "claude",
			name:           "GLM",
			settingsConfig: makeClaudeSettings("https://open.bigmodel.cn/api/paas/v4", "sk-key", "glm-4v"),
		},
		{
			id:             "mimo-1",
			appType:        "claude",
			name:           "MiMo",
			settingsConfig: makeClaudeSettings("https://api.xiaomimimo.com/anthropic", "sk-key", "mimo-v2.5"),
		},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 3 {
		t.Fatalf("expected 3 providers (none self-referential), got %d: %+v", len(providers), providers)
	}

	// Verify all expected names are present.
	names := map[string]bool{}
	for _, p := range providers {
		names[p.Name] = true
	}
	for _, want := range []string{"DeepSeek", "GLM", "MiMo"} {
		if !names[want] {
			t.Errorf("expected provider %q in result, got: %+v", want, providers)
		}
	}
}

// P1-20: ImportFromCcSwitch should filter providers whose base_url uses the
// "localhost" alias (which normalizes to 127.0.0.1). This catches the common
// case where users type "localhost" instead of "127.0.0.1" in cc-switch.
func TestImportFromCcSwitch_FiltersLocalhostAlias(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-localhost",
			appType:        "claude",
			name:           "SelfRefLocalhost",
			settingsConfig: makeClaudeSettings("http://localhost:8790/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	// Filter with 127.0.0.1:8790 — should catch the localhost alias.
	providers, err := ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider after filtering localhost alias, got %d: %+v", len(providers), providers)
	}
	if providers[0].Name != "DeepSeek" {
		t.Errorf("expected DeepSeek to remain, got %q", providers[0].Name)
	}

	// Reverse direction: filter with localhost:8790 — should catch 127.0.0.1 URL.
	rows2 := []ccSwitchTestRow{
		{
			id:             "deepseek-2",
			appType:        "claude",
			name:           "DeepSeek2",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-127",
			appType:        "claude",
			name:           "SelfRef127",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	dbPath2 := filepath.Join(dir, "cc-switch2.db")
	createTestDB(t, dbPath2, rows2)

	providers2, err := ImportFromCcSwitch(dbPath2, "localhost:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch (reverse): %v", err)
	}
	if len(providers2) != 1 {
		t.Fatalf("expected 1 provider after filtering 127.0.0.1 URL with localhost listen, got %d", len(providers2))
	}
	if providers2[0].Name != "DeepSeek2" {
		t.Errorf("expected DeepSeek2 to remain, got %q", providers2[0].Name)
	}
}

// P1-21: ImportFromCcSwitch should filter providers whose base_url uses the
// ::1 IPv6 loopback alias (which normalizes to 127.0.0.1).
func TestImportFromCcSwitch_FiltersIPv6Alias(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-ipv6",
			appType:        "claude",
			name:           "SelfRefIPv6",
			settingsConfig: makeClaudeSettings("http://[::1]:8790/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	// Filter with 127.0.0.1:8790 — should catch the ::1 IPv6 alias.
	providers, err := ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider after filtering IPv6 alias, got %d: %+v", len(providers), providers)
	}
	if providers[0].Name != "DeepSeek" {
		t.Errorf("expected DeepSeek to remain, got %q", providers[0].Name)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// P2: IsSelfReferentialURL URL decoration edge cases
// ──────────────────────────────────────────────────────────────────────────

// P2-1: trailing slash on the URL should not affect self-loop detection —
// "http://127.0.0.1:8790/" and "http://127.0.0.1:8790" are equivalent.
func TestIsSelfReferentialURL_TrailingSlash(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"trailing-slash-url", "http://127.0.0.1:8790/", "127.0.0.1:8790", true},
		{"no-slash-url", "http://127.0.0.1:8790", "127.0.0.1:8790", true},
		{"trailing-slash-with-path", "http://127.0.0.1:8790/anthropic/", "127.0.0.1:8790", true},
		{"multiple-trailing-slashes", "http://127.0.0.1:8790//", "127.0.0.1:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-2: query parameters should not affect self-loop detection — the host:port
// is what matters, not the query string.
func TestIsSelfReferentialURL_QueryParams(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"query-param", "http://127.0.0.1:8790/anthropic?key=val", "127.0.0.1:8790", true},
		{"multiple-params", "http://127.0.0.1:8790/anthropic?a=1&b=2&c=3", "127.0.0.1:8790", true},
		{"empty-query", "http://127.0.0.1:8790/anthropic?", "127.0.0.1:8790", true},
		{"query-only-no-path", "http://127.0.0.1:8790?key=val", "127.0.0.1:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-3: URL fragments should not affect self-loop detection — fragments are
// client-side only and never sent to the server.
func TestIsSelfReferentialURL_Fragment(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"fragment", "http://127.0.0.1:8790/anthropic#section", "127.0.0.1:8790", true},
		{"fragment-only", "http://127.0.0.1:8790#", "127.0.0.1:8790", true},
		{"fragment-and-query", "http://127.0.0.1:8790/anthropic?key=val#section", "127.0.0.1:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-4: uppercase host should not bypass detection — "HTTP://127.0.0.1:8790"
// and "HTTP://LOCALHOST:8790" are equivalent to their lowercase forms.
func TestIsSelfReferentialURL_UppercaseHost(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"uppercase-scheme", "HTTP://127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
		{"uppercase-host-localhost", "http://LOCALHOST:8790/anthropic", "127.0.0.1:8790", true},
		{"mixed-case-host", "http://Localhost:8790/anthropic", "127.0.0.1:8790", true},
		{"uppercase-host-127", "http://127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-5: URL with userinfo (user:pass@host) should still be detected —
// the userinfo is stripped before host:port comparison.
func TestIsSelfReferentialURL_UserInfo(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"user-only", "http://user@127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
		{"user-pass", "http://user:pass@127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
		{"user-localhost", "http://user:pass@localhost:8790/anthropic", "127.0.0.1:8790", true},
		{"empty-user", "http://@127.0.0.1:8790/anthropic", "127.0.0.1:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-6: extreme port values (0 and 65535) should still be detected — these
// are valid port numbers and should not bypass the check.
func TestIsSelfReferentialURL_ExtremePorts(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		{"port-0", "http://127.0.0.1:0/anthropic", "127.0.0.1:0", true},
		{"port-65535", "http://127.0.0.1:65535/anthropic", "127.0.0.1:65535", true},
		{"port-0-vs-65535", "http://127.0.0.1:0/anthropic", "127.0.0.1:65535", false},
		{"port-1", "http://127.0.0.1:1/anthropic", "127.0.0.1:1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P0-7: port leading zeros must not bypass the self-loop check. Go's net/http
// parses "08790" as port 8790 (strconv.Atoi accepts leading zeros), so a bare
// string comparison of "08790" vs "8790" would miss the loop. The fix
// normalizes numeric ports via integer comparison.
func TestIsSelfReferentialURL_PortLeadingZeros(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		// Leading-zero URL port matches canonical listen port (the loop is real).
		{"url-leading-zero-matches", "http://127.0.0.1:08790/anthropic", "127.0.0.1:8790", true},
		// Leading-zero listen port matches canonical URL port (reverse direction).
		{"listen-leading-zero-matches", "http://127.0.0.1:8790/anthropic", "127.0.0.1:08790", true},
		// Both sides leading zeros, same numeric port.
		{"both-leading-zeros", "http://127.0.0.1:08790/anthropic", "127.0.0.1:08790", true},
		// Leading-zero URL port but DIFFERENT numeric port — must NOT match.
		{"leading-zero-different-port", "http://127.0.0.1:08080/anthropic", "127.0.0.1:8790", false},
		// Heavier leading zeros.
		{"heavy-leading-zeros", "http://127.0.0.1:00080/anthropic", "127.0.0.1:80", true},
		// Default-port URL (no port) vs leading-zero listen port on 80.
		{"default-port-vs-leading-zero", "http://127.0.0.1/anthropic", "127.0.0.1:080", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P0-8: a trailing dot in the host (the DNS fully-qualified-name marker, e.g.
// "127.0.0.1.") must not bypass the self-loop check. Go's resolver treats
// "127.0.0.1." as 127.0.0.1, so without stripping the trailing dot the check
// would miss the loop. This also covers "localhost." variants.
func TestIsSelfReferentialURL_TrailingDotHost(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		// URL host with trailing dot matches loopback listen.
		{"url-ip-trailing-dot", "http://127.0.0.1.:8790/anthropic", "127.0.0.1:8790", true},
		// Listen host with trailing dot matches canonical URL host.
		{"listen-ip-trailing-dot", "http://127.0.0.1:8790/anthropic", "127.0.0.1.:8790", true},
		// localhost. trailing dot.
		{"localhost-trailing-dot", "http://localhost.:8790/anthropic", "127.0.0.1:8790", true},
		// 0.0.0.0. trailing dot.
		{"zero-bound-trailing-dot", "http://0.0.0.0.:8790/anthropic", "127.0.0.1:8790", true},
		// Both sides trailing dots.
		{"both-trailing-dot", "http://127.0.0.1.:8790/anthropic", "127.0.0.1.:8790", true},
		// Trailing dot but DIFFERENT port — must NOT match.
		{"trailing-dot-different-port", "http://127.0.0.1.:8080/anthropic", "127.0.0.1:8790", false},
		// External host with trailing dot is NOT self-referential.
		{"external-trailing-dot", "http://api.deepseek.com.:8790/anthropic", "127.0.0.1:8790", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P2-11: when listen is empty, ImportFromCcSwitch should NOT filter anything —
// this preserves backward compatibility for callers that don't know the
// proxy's listen address (e.g. external tooling).
func TestImportFromCcSwitch_EmptyListen_NoFilter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		{
			id:             "self-1",
			appType:        "claude",
			name:           "SelfRef1",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "self-2",
			appType:        "claude",
			name:           "SelfRef2",
			settingsConfig: makeClaudeSettings("http://localhost:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	// Empty listen → no filtering, all 3 returned.
	providers, err := ImportFromCcSwitch(dbPath, "")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 3 {
		t.Errorf("expected 3 providers with empty listen (no filter), got %d", len(providers))
	}
}

// P2-12: mixed port variants — only providers on the SAME port as listen are
// filtered; providers on different ports (even self-referential host) are kept.
func TestImportFromCcSwitch_MixedPortVariants(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cc-switch.db")

	rows := []ccSwitchTestRow{
		// Self-referential on 8790 (same as listen) → filtered
		{
			id:             "self-8790",
			appType:        "claude",
			name:           "SelfRef8790",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8790/anthropic", "sk-key", "deepseek-chat"),
		},
		// Self-referential host but different port (8080) → kept (not a loop)
		{
			id:             "self-8080",
			appType:        "claude",
			name:           "SelfRef8080",
			settingsConfig: makeClaudeSettings("http://127.0.0.1:8080/anthropic", "sk-key", "deepseek-chat"),
		},
		// External provider → kept
		{
			id:             "deepseek-1",
			appType:        "claude",
			name:           "DeepSeek",
			settingsConfig: makeClaudeSettings("https://api.deepseek.com/anthropic", "sk-key", "deepseek-chat"),
		},
	}
	createTestDB(t, dbPath, rows)

	providers, err := ImportFromCcSwitch(dbPath, "127.0.0.1:8790")
	if err != nil {
		t.Fatalf("ImportFromCcSwitch: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers (self-8790 filtered, self-8080 kept), got %d: %+v", len(providers), providers)
	}

	names := map[string]bool{}
	for _, p := range providers {
		names[p.Name] = true
	}
	if !names["SelfRef8080"] {
		t.Errorf("SelfRef8080 (different port) should be kept, got: %+v", providers)
	}
	if !names["DeepSeek"] {
		t.Errorf("DeepSeek should be kept, got: %+v", providers)
	}
	if names["SelfRef8790"] {
		t.Errorf("SelfRef8790 (same port) should be filtered, got: %+v", providers)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// P3: IsSelfReferentialURL extreme/defensive inputs
// ──────────────────────────────────────────────────────────────────────────

// P3-1: malformed URLs should return false (not panic) — the function must
// never crash on bad input, since it's called from config-loading paths.
func TestIsSelfReferentialURL_MalformedURL(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
	}{
		{"empty-string", "", "127.0.0.1:8790"},
		{"no-scheme", "127.0.0.1:8790/anthropic", "127.0.0.1:8790"},
		{"scheme-only", "http://", "127.0.0.1:8790"},
		{"garbage", "://not-a-url", "127.0.0.1:8790"},
		{"spaces", "http:// 127.0.0.1:8790/anthropic", "127.0.0.1:8790"},
		{"control-chars", "http://\x00:8790", "127.0.0.1:8790"},
		{"port-only", "http://:8790", "127.0.0.1:8790"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Should not panic; should return false (safe default).
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("IsSelfReferentialURL(%q, %q) panicked: %v", c.urlStr, c.listen, r)
				}
			}()
			got := IsSelfReferentialURL(c.urlStr, c.listen)
			// Malformed URLs must return false — never true (would cause
			// false positives that block valid configs).
			if got {
				t.Errorf("IsSelfReferentialURL(%q, %q) = true, want false (malformed URL should be safe-default)", c.urlStr, c.listen)
			}
		})
	}
}

// P3-2: empty inputs (empty URL, empty listen, both empty) should return false.
func TestIsSelfReferentialURL_EmptyInputs(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
	}{
		{"both-empty", "", ""},
		{"empty-url", "", "127.0.0.1:8790"},
		{"empty-listen", "http://127.0.0.1:8790", ""},
		{"empty-url-with-listen", "  ", "127.0.0.1:8790"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got {
				t.Errorf("IsSelfReferentialURL(%q, %q) = true, want false (empty input)", c.urlStr, c.listen)
			}
		})
	}
}

// P3-3: a hostname (not an IP) in the URL should NOT match an IP listen
// address — "localhost" matches 127.0.0.1 (alias), but "myproxy.local" does not.
func TestIsSelfReferentialURL_HostnameNotIP(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		// hostname that is NOT a known loopback alias → no match
		{"hostname-vs-ip", "http://myproxy.local:8790/anthropic", "127.0.0.1:8790", false},
		{"deepseek-hostname", "https://api.deepseek.com/anthropic", "127.0.0.1:8790", false},
		// "localhost" IS a known alias → matches 127.0.0.1
		{"localhost-alias", "http://localhost:8790/anthropic", "127.0.0.1:8790", true},
		// hostname vs hostname (same name) → match
		{"same-hostname", "http://myproxy.local:8790/anthropic", "myproxy.local:8790", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}

// P3-4: IPv6 addresses in full form (with brackets, with zone ID) should be
// detected correctly — IPv6 is tricky because of bracketing rules.
func TestIsSelfReferentialURL_IPv6FullForm(t *testing.T) {
	cases := []struct {
		name   string
		urlStr string
		listen string
		want   bool
	}{
		// ::1 (loopback) in bracketed form
		{"ipv6-loopback-bracketed", "http://[::1]:8790/anthropic", "127.0.0.1:8790", true},
		// ::1 vs ::1 (same IPv6)
		{"ipv6-same", "http://[::1]:8790/anthropic", "[::1]:8790", true},
		// Full IPv6 address (2001:db8::1) — not a loopback, no match
		{"ipv6-non-loopback", "http://[2001:db8::1]:8790/anthropic", "127.0.0.1:8790", false},
		// IPv6 with zone ID (fe80::1%eth0) — should not match 127.0.0.1
		{"ipv6-zone-id", "http://[fe80::1%25eth0]:8790/anthropic", "127.0.0.1:8790", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSelfReferentialURL(c.urlStr, c.listen); got != c.want {
				t.Errorf("IsSelfReferentialURL(%q, %q) = %v, want %v", c.urlStr, c.listen, got, c.want)
			}
		})
	}
}
