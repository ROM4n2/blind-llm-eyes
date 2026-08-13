package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runStatus reports whether the proxy is running by reading the pidfile and
// probing /healthz. Exit 0 = RUNNING, 1 = STALE or not running.
func runStatus(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := DefaultPidfilePath()
	if err != nil {
		fmt.Fprintf(stderr, "locate pidfile: %v\n", err)
		return 1
	}
	return printStatus(path, stdout, stderr)
}

// printStatus is the testable core: it reads the pidfile at pidfilePath and
// probes /healthz to distinguish RUNNING from STALE.
func printStatus(pidfilePath string, stdout, stderr io.Writer) int {
	data, err := ReadPidfile(pidfilePath)
	if os.IsNotExist(err) {
		fmt.Fprintln(stdout, "not running (no pidfile found)")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "read pidfile: %v\n", err)
		return 1
	}
	if pingHealthz(data.Addr, 2*time.Second) {
		fmt.Fprintf(stdout, "RUNNING  pid=%d  addr=%s  started=%s\n",
			data.PID, data.Addr, data.StartedAt.Format(time.RFC3339))
		return 0
	}
	fmt.Fprintf(stdout, "STALE  pid=%d  addr=%s  (pidfile exists but /healthz unreachable)\n", data.PID, data.Addr)
	fmt.Fprintln(stderr, "hint: run 'blind-llm-eyes stop' to shut it down, or remove the pidfile manually:")
	fmt.Fprintf(stderr, "  %s\n", pidfilePath)
	return 1
}

// pingHealthz returns true if GET http://addr/healthz responds 200 within timeout.
// We do not trust the OS pid (Windows reuses pids); only a live healthz probe counts.
func pingHealthz(addr string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
