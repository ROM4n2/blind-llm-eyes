package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PidfileData is the JSON content of the pidfile, read by the status/stop
// subcommands to locate and authenticate to the running server.
type PidfileData struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	Token     string    `json:"token"`
	StartedAt time.Time `json:"started_at"`
}

// DefaultPidfilePath returns the conventional pidfile location under the OS
// user config dir: <UserConfigDir>/blind-llm-eyes/pidfile.json.
func DefaultPidfilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	return filepath.Join(dir, "blind-llm-eyes", "pidfile.json"), nil
}

// WritePidfile atomically writes the pidfile (write to temp + rename), creating
// parent directories as needed.
//
// Implementation note: we use os.WriteFile to a fixed temp path (path+".tmp")
// instead of os.CreateTemp. The Trae IDE sandbox blocks os.CreateTemp in
// %AppData% directories, causing status/stop subcommands to fail when run
// from the IDE's integrated terminal. A fixed-name temp file + rename achieves
// the same atomic-write semantics without triggering the sandbox restriction.
func WritePidfile(path string, data PidfileData) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create pidfile dir: %w", err)
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	// Write to a fixed-name temp file, then rename for atomicity.
	// This avoids os.CreateTemp which is blocked by the Trae IDE sandbox.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return fmt.Errorf("write temp pidfile: %w", err)
	}
	return os.Rename(tmpPath, path)
}

// ReadPidfile reads and parses the pidfile.
func ReadPidfile(path string) (PidfileData, error) {
	var data PidfileData
	raw, err := os.ReadFile(path)
	if err != nil {
		return data, err
	}
	return data, json.Unmarshal(raw, &data)
}
