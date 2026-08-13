package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestPidfile(t *testing.T, path, addr, token string) {
	t.Helper()
	if err := WritePidfile(path, PidfileData{PID: 4242, Addr: addr, Token: token}); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
}

func TestPrintStatus_NoPidfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := printStatus(filepath.Join(t.TempDir(), "nope.json"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("expected 'not running', got %q", stdout.String())
	}
}

func TestPrintStatus_Running(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	path := filepath.Join(t.TempDir(), "pidfile.json")
	writeTestPidfile(t, path, addr, "tok")

	var stdout, stderr bytes.Buffer
	code := printStatus(path, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "RUNNING") {
		t.Errorf("expected RUNNING, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), addr) {
		t.Errorf("expected addr %s in output, got %q", addr, stdout.String())
	}
}

func TestPrintStatus_Stale(t *testing.T) {
	// Port 1 is privileged and nothing listens → connection refused quickly.
	path := filepath.Join(t.TempDir(), "pidfile.json")
	writeTestPidfile(t, path, "127.0.0.1:1", "tok")

	var stdout, stderr bytes.Buffer
	code := printStatus(path, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 for stale, got %d", code)
	}
	if !strings.Contains(stdout.String(), "STALE") {
		t.Errorf("expected STALE, got %q", stdout.String())
	}
}

func TestPingHealthz_Unreachable(t *testing.T) {
	if pingHealthz("127.0.0.1:1", 500*1000*1000) { // 500ms
		t.Fatal("expected healthz to be unreachable on port 1")
	}
}
