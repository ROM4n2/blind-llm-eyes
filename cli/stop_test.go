package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoStop_NoPidfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doStop(filepath.Join(t.TempDir(), "nope.json"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), "not running") {
		t.Errorf("expected 'not running', got %q", stdout.String())
	}
}

func TestDoStop_Unreachable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pidfile.json")
	writeTestPidfile(t, path, "127.0.0.1:1", "tok")
	var stdout, stderr bytes.Buffer
	code := doStop(path, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 when unreachable, got %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot reach server") {
		t.Errorf("expected 'cannot reach server', got %q", stderr.String())
	}
}

func TestDoStop_Success(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pidfile.json")
	shutdownReceived := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Token") != "tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		close(shutdownReceived)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	writeTestPidfile(t, path, addr, "tok")

	// Simulate the real server removing its pidfile during graceful shutdown.
	go func() {
		<-shutdownReceived
		os.Remove(path)
	}()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- doStop(path, &stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "stopped") {
			t.Errorf("expected 'stopped', got %q", stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doStop did not return within 5s")
	}
}

func TestPostShutdown_WrongToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := postShutdown(addr, "wrong", time.Second); err == nil {
		t.Fatal("expected error for wrong token")
	}
}

func TestWaitForPidfileGone_RemovedMidWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pidfile.json")
	writeTestPidfile(t, path, "127.0.0.1:1", "tok")
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Remove(path)
	}()
	if !waitForPidfileGone(path, 2*time.Second) {
		t.Fatal("expected pidfile to be gone")
	}
}

func TestWaitForPidfileGone_Timeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pidfile.json")
	writeTestPidfile(t, path, "127.0.0.1:1", "tok")
	if waitForPidfileGone(path, 300*time.Millisecond) {
		t.Fatal("expected timeout, pidfile still present")
	}
}
