package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runStop reads the pidfile, POSTs /admin/shutdown with the stored token, and
// waits for the pidfile to disappear (the server removes it during graceful
// shutdown). Exit 0 = stopped, 1 = not running / unreachable / timed out.
func runStop(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := DefaultPidfilePath()
	if err != nil {
		fmt.Fprintf(stderr, "locate pidfile: %v\n", err)
		return 1
	}
	return doStop(path, stdout, stderr)
}

// doStop is the testable core.
func doStop(pidfilePath string, stdout, stderr io.Writer) int {
	data, err := ReadPidfile(pidfilePath)
	if os.IsNotExist(err) {
		fmt.Fprintln(stdout, "not running (no pidfile found)")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "read pidfile: %v\n", err)
		return 1
	}
	if err := postShutdown(data.Addr, data.Token, 5*time.Second); err != nil {
		fmt.Fprintf(stderr, "cannot reach server at %s: %v\n", data.Addr, err)
		fmt.Fprintf(stderr, "the pidfile may be stale; remove it manually:\n  %s\n", pidfilePath)
		return 1
	}
	fmt.Fprintln(stdout, "shutdown requested, waiting for server to stop...")
	if !waitForPidfileGone(pidfilePath, 10*time.Second) {
		fmt.Fprintln(stderr, "server did not remove pidfile within 10s; it may still be draining or the pidfile is stale")
		return 1
	}
	fmt.Fprintln(stdout, "stopped")
	return 0
}

// postShutdown POSTs /admin/shutdown with the admin token. Returns nil on 202.
func postShutdown(addr, token string, timeout time.Duration) error {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Admin-Token", token)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("admin shutdown returned %s", resp.Status)
	}
	return nil
}

// waitForPidfileGone polls for the pidfile's removal up to timeout.
func waitForPidfileGone(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
