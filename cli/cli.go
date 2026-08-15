// Package cli implements the blind-llm-eyes subcommand dispatch.
//
// Run is the testable entry point: it takes the argument list (excluding the
// program name), the standard streams, and returns a process exit code. main.go
// delegates to Run for every non-server subcommand (the server path — no args,
// "start", or -flags — stays in main.go's runServer).
package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/ROM4n2/blind-llm-eyes/buildinfo"
)

// Run dispatches a blind-llm-eyes subcommand.
//
// args is the full argument list excluding the program name (os.Args[1:]).
// It returns the process exit code (0 on success, non-zero on failure).
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "version":
		return runVersion(stdout)
	case "start":
		// "start" is normally intercepted by main.go's runServer. If it reaches
		// here (e.g. invoked via cli.Run directly), advise the user.
		fmt.Fprintln(stderr, "the server is launched by running 'blind-llm-eyes' or 'blind-llm-eyes start'")
		return 0
	case "setup":
		return runSetup(rest, stdin, stdout, stderr)
	case "doctor":
		return runDoctor(rest, stdin, stdout, stderr)
	case "connect":
		return runConnect(rest, stdin, stdout, stderr)
	case "disconnect":
		return runDisconnect(rest, stdin, stdout, stderr)
	case "status":
		return runStatus(rest, stdin, stdout, stderr)
	case "stop":
		return runStop(rest, stdin, stdout, stderr)
	case "cache":
		return runCache(rest, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "blind-llm-eyes: unknown command %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}
}

// runVersion prints "blind-llm-eyes <Version> (go <runtime>)".
func runVersion(stdout io.Writer) int {
	fmt.Fprintf(stdout, "blind-llm-eyes %s (go %s)\n", buildinfo.Version, runtime.Version())
	return 0
}

// notImplemented reports that a subcommand is not yet available.
func notImplemented(cmd string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "blind-llm-eyes %s: not implemented yet\n", cmd)
	return 2
}

// printUsage writes the command summary to w.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: blind-llm-eyes <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  start        Run the proxy server in the foreground")
	fmt.Fprintln(w, "  setup        Interactive configuration wizard")
	fmt.Fprintln(w, "  doctor       Run connectivity self-checks")
	fmt.Fprintln(w, "  connect      Wire Claude Code to this proxy")
	fmt.Fprintln(w, "  disconnect   Restore Claude Code settings")
	fmt.Fprintln(w, "  status       Show running proxy status")
	fmt.Fprintln(w, "  stop         Stop the running proxy")
	fmt.Fprintln(w, "  cache        Manage persistent cache (stats/list/clear/path)")
	fmt.Fprintln(w, "  version      Print version information")
}
