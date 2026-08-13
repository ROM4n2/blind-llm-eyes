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
func WritePidfile(path string, data PidfileData) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create pidfile dir: %w", err)
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "pidfile-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp pidfile: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp pidfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
