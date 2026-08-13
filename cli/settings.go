package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// backupMarker is the sentinel content written to the backup file when the
// original settings.json did not exist. On disconnect, an empty backup means
// "remove settings.json to restore the pre-connect state".
const backupMarker = ""

// connectSettings reads settingsPath, backs it up to backupPath (only if the
// backup doesn't already exist), sets env.ANTHROPIC_BASE_URL to proxyURL, and
// writes the result atomically.
//
// If settingsPath doesn't exist, a new file is created with just the env key,
// and an empty backup marker is written so disconnect can remove the file.
//
// Non-env top-level keys are preserved byte-for-byte (via json.RawMessage
// round-trip). Within env, all existing keys are preserved; only
// ANTHROPIC_BASE_URL is set/updated.
func connectSettings(settingsPath, backupPath, proxyURL string) error {
	original, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read settings: %w", err)
	}
	existed := !os.IsNotExist(err)

	// ── Backup ──
	// Only create backup if it doesn't already exist (don't overwrite a
	// previous backup from an earlier connect).
	if _, berr := os.Stat(backupPath); os.IsNotExist(berr) {
		if existed {
			if werr := os.WriteFile(backupPath, original, 0644); werr != nil {
				return fmt.Errorf("write backup: %w", werr)
			}
		} else {
			// Write empty marker so disconnect knows to remove settings.json
			if werr := os.WriteFile(backupPath, []byte(backupMarker), 0644); werr != nil {
				return fmt.Errorf("write backup marker: %w", werr)
			}
		}
	}

	// ── Parse + modify ──
	var top map[string]json.RawMessage
	if existed {
		if err := json.Unmarshal(original, &top); err != nil {
			return fmt.Errorf("parse settings JSON: %w", err)
		}
	} else {
		top = make(map[string]json.RawMessage)
	}

	// Parse env (or create empty)
	var env map[string]json.RawMessage
	if envRaw, ok := top["env"]; ok {
		if err := json.Unmarshal(envRaw, &env); err != nil {
			return fmt.Errorf("parse env: %w", err)
		}
	} else {
		env = make(map[string]json.RawMessage)
	}

	// Set ANTHROPIC_BASE_URL
	urlBytes, _ := json.Marshal(proxyURL)
	env["ANTHROPIC_BASE_URL"] = json.RawMessage(urlBytes)

	// Serialize env back
	envBytes, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	top["env"] = json.RawMessage(envBytes)

	// Serialize top-level
	result, err := json.MarshalIndent(top, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	result = append(result, '\n')

	// ── Atomic write ──
	return atomicWrite(settingsPath, result)
}

// disconnectSettings restores settingsPath from backupPath.
//
// If the backup is empty (marker for "no original existed"), settingsPath is
// removed. If the backup has content, it is copied byte-for-byte to
// settingsPath.
//
// Returns an error if the backup doesn't exist (never connected or already
// disconnected).
func disconnectSettings(settingsPath, backupPath string) error {
	backup, err := os.ReadFile(backupPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("no backup found at %s — not connected or already disconnected", backupPath)
	}
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}

	if len(backup) == 0 {
		// Marker: no original settings.json existed → remove current
		if rerr := os.Remove(settingsPath); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("remove settings: %w", rerr)
		}
		return nil
	}

	// Restore byte-for-byte
	return atomicWrite(settingsPath, backup)
}

// atomicWrite writes data to path via a temp file + rename, creating parent
// directories as needed.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// defaultSettingsPath returns ~/.claude/settings.json (cross-platform).
func defaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// defaultBackupPath returns ~/.claude/.bak-before-connect.
func defaultBackupPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home dir: %w", err)
	}
	return filepath.Join(home, ".claude", ".bak-before-connect"), nil
}
