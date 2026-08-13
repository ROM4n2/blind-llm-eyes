package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile writes data to path, creating parent dirs.
func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readTestFile reads a file, failing the test on error.
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestConnectSettings_PreservesNonEnvKeys(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	original := `{"effortLevel":"high","model":"claude-sonnet-4-20250514","theme":"dark","env":{"ANTHROPIC_AUTH_TOKEN":"sk-old","SOME_OTHER":"val"}}`
	writeTestFile(t, settingsPath, original)

	if err := connectSettings(settingsPath, backupPath, "http://127.0.0.1:8790"); err != nil {
		t.Fatalf("connectSettings: %v", err)
	}

	// Read result
	result := readTestFile(t, settingsPath)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Non-env keys must be present and unchanged
	if string(m["effortLevel"]) != `"high"` {
		t.Errorf("effortLevel: got %s, want \"high\"", m["effortLevel"])
	}
	if string(m["model"]) != `"claude-sonnet-4-20250514"` {
		t.Errorf("model: got %s", m["model"])
	}
	if string(m["theme"]) != `"dark"` {
		t.Errorf("theme: got %s", m["theme"])
	}

	// env must have ANTHROPIC_BASE_URL set to proxy URL
	var env map[string]json.RawMessage
	if err := json.Unmarshal(m["env"], &env); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	if string(env["ANTHROPIC_BASE_URL"]) != `"http://127.0.0.1:8790"` {
		t.Errorf("ANTHROPIC_BASE_URL: got %s, want \"http://127.0.0.1:8790\"", env["ANTHROPIC_BASE_URL"])
	}
	// Other env keys preserved
	if string(env["ANTHROPIC_AUTH_TOKEN"]) != `"sk-old"` {
		t.Errorf("ANTHROPIC_AUTH_TOKEN: got %s, want \"sk-old\"", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if string(env["SOME_OTHER"]) != `"val"` {
		t.Errorf("SOME_OTHER: got %s, want \"val\"", env["SOME_OTHER"])
	}

	// Backup must exist and match original
	backup := readTestFile(t, backupPath)
	if string(backup) != original {
		t.Errorf("backup mismatch:\n got: %s\nwant: %s", backup, original)
	}
}

func TestConnectSettings_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	// No existing settings.json
	if err := connectSettings(settingsPath, backupPath, "http://127.0.0.1:8790"); err != nil {
		t.Fatalf("connectSettings: %v", err)
	}

	result := readTestFile(t, settingsPath)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(m["env"], &env); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	if string(env["ANTHROPIC_BASE_URL"]) != `"http://127.0.0.1:8790"` {
		t.Errorf("ANTHROPIC_BASE_URL: got %s", env["ANTHROPIC_BASE_URL"])
	}

	// An empty backup marker should be created so disconnect knows to remove
	// the created settings.json
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup marker should exist: %v", err)
	}
	if len(backup) != 0 {
		t.Errorf("backup marker should be empty, got %q", backup)
	}
}

func TestConnectSettings_DoesNotOverwriteBackup(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	original := `{"model":"original-model","env":{"ANTHROPIC_BASE_URL":"http://old:1234"}}`
	writeTestFile(t, settingsPath, original)

	// Pre-existing backup
	existingBackup := `{"model":"backup-model","env":{}}`
	writeTestFile(t, backupPath, existingBackup)

	if err := connectSettings(settingsPath, backupPath, "http://127.0.0.1:8790"); err != nil {
		t.Fatalf("connectSettings: %v", err)
	}

	// Backup must NOT be overwritten
	backup := readTestFile(t, backupPath)
	if string(backup) != existingBackup {
		t.Errorf("backup was overwritten:\n got: %s\nwant: %s", backup, existingBackup)
	}
}

func TestConnectSettings_UpdatesExistingBaseURL(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	original := `{"env":{"ANTHROPIC_BASE_URL":"http://old:1234","ANTHROPIC_AUTH_TOKEN":"tok"}}`
	writeTestFile(t, settingsPath, original)

	if err := connectSettings(settingsPath, backupPath, "http://127.0.0.1:8790"); err != nil {
		t.Fatalf("connectSettings: %v", err)
	}

	result := readTestFile(t, settingsPath)
	var m map[string]json.RawMessage
	json.Unmarshal(result, &m)
	var env map[string]json.RawMessage
	json.Unmarshal(m["env"], &env)

	if string(env["ANTHROPIC_BASE_URL"]) != `"http://127.0.0.1:8790"` {
		t.Errorf("ANTHROPIC_BASE_URL: got %s", env["ANTHROPIC_BASE_URL"])
	}
	if string(env["ANTHROPIC_AUTH_TOKEN"]) != `"tok"` {
		t.Errorf("ANTHROPIC_AUTH_TOKEN should be preserved: got %s", env["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestDisconnectSettings_RestoresFromBackup(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	original := `{"model":"original","theme":"dark","env":{"ANTHROPIC_BASE_URL":"http://original:1234"}}`
	backupContent := `{"model":"backup","theme":"light","env":{}}`

	// Current settings.json is modified (after connect)
	writeTestFile(t, settingsPath, original)
	// Backup contains the pre-connect state
	writeTestFile(t, backupPath, backupContent)

	if err := disconnectSettings(settingsPath, backupPath); err != nil {
		t.Fatalf("disconnectSettings: %v", err)
	}

	// settings.json must be byte-identical to backup
	result := readTestFile(t, settingsPath)
	if string(result) != backupContent {
		t.Errorf("settings.json after disconnect:\n got: %s\nwant: %s", result, backupContent)
	}
}

func TestDisconnectSettings_NoBackup_Error(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	writeTestFile(t, settingsPath, `{"model":"x"}`)
	// No backup file

	err := disconnectSettings(settingsPath, backupPath)
	if err == nil {
		t.Fatal("expected error when no backup exists")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("error should mention backup, got: %v", err)
	}
}

func TestDisconnectSettings_RemovesSettingsIfNoOriginal(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")

	// Simulate: connect created settings.json (no original existed),
	// so backup is an empty file marker
	writeTestFile(t, settingsPath, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8790"}}`)
	writeTestFile(t, backupPath, "") // empty backup = no original existed

	if err := disconnectSettings(settingsPath, backupPath); err != nil {
		t.Fatalf("disconnectSettings: %v", err)
	}

	// settings.json should be removed (restored to non-existent state)
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings.json should be removed after disconnect (no original), stat err=%v", err)
	}
}

func TestRunConnect_FlagParsing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runConnect([]string{"-config", "nonexistent.yaml"}, nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit for missing config")
	}
	if !strings.Contains(stderr.String(), "nonexistent.yaml") {
		t.Errorf("expected error to mention config file, got: %s", stderr.String())
	}
}

func TestRunDisconnect_FlagParsing(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	backupPath := filepath.Join(dir, ".bak-before-connect")
	// No backup exists

	var stdout, stderr bytes.Buffer
	code := runDisconnect([]string{"-settings", settingsPath, "-backup", backupPath}, nil, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when no backup exists")
	}
	if !strings.Contains(stderr.String(), "backup") {
		t.Errorf("expected error to mention backup, got: %s", stderr.String())
	}
}
