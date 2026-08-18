package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runReload reads the pidfile, POSTs /admin/reload with the stored token,
// and prints the server's response. Exit 0 = reloaded, 1 = not running /
// unreachable / reload failed.
func runReload(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := DefaultPidfilePath()
	if err != nil {
		fmt.Fprintf(stderr, "locate pidfile: %v\n", err)
		return 1
	}
	return doReload(path, stdout, stderr)
}

// doReload is the testable core.
func doReload(pidfilePath string, stdout, stderr io.Writer) int {
	data, err := ReadPidfile(pidfilePath)
	if os.IsNotExist(err) {
		fmt.Fprintln(stdout, "not running (no pidfile found)")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "read pidfile: %v\n", err)
		return 1
	}
	body, status, err := postReload(data.Addr, data.Token, 10*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "cannot reach server at %s: %v\n", data.Addr, err)
		fmt.Fprintf(stderr, "the pidfile may be stale; remove it manually:\n  %s\n", pidfilePath)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "reload failed (HTTP %d):\n%s\n", status, body)
		return 1
	}
	fmt.Fprintln(stdout, "reload successful:")
	fmt.Fprint(stdout, body)
	return 0
}

// postReload POSTs /admin/reload with the admin token. Returns the response
// body, status code, and any transport-level error.
func postReload(addr, token string, timeout time.Duration) (string, int, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/admin/reload", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("X-Admin-Token", token)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(raw), resp.StatusCode, nil
}
