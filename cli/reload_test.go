package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoReload_NoPidfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pidfile.json")
	code := doReload(path, os.Stdout, os.Stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 when no pidfile, got %d", code)
	}
}

func TestDoReload_ServerReturnsOK(t *testing.T) {
	// Stand up a fake admin server that returns 200 + JSON body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/reload" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Admin-Token") != "tok123" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","prev_fingerprint":"aaa","next_fingerprint":"bbb"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "pidfile.json")
	// Extract host:port from srv.URL (strips "http://")
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := WritePidfile(path, PidfileData{
		PID:   1,
		Addr:  addr,
		Token: "tok123",
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := doReload(path, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reload successful") {
		t.Errorf("stdout should mention success, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "next_fingerprint") {
		t.Errorf("stdout should contain fingerprint, got %q", stdout.String())
	}
}

func TestDoReload_ServerReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("reload failed: field 'listen' is non-reloadable"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "pidfile.json")
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := WritePidfile(path, PidfileData{
		PID:   1,
		Addr:  addr,
		Token: "tok123",
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := doReload(path, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on server error, got %d", code)
	}
	if !strings.Contains(stderr.String(), "reload failed") {
		t.Errorf("stderr should mention failure, got %q", stderr.String())
	}
}

func TestDoReload_WrongToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "pidfile.json")
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := WritePidfile(path, PidfileData{
		PID:   1,
		Addr:  addr,
		Token: "wrong-token",
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := doReload(path, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 on 403, got %d", code)
	}
}
