// Package cli implements the blind-llm-eyes subcommand dispatch.
//
// Run is the testable entry point: it takes the argument list (excluding the
// program name), the standard streams, and returns a process exit code. main.go
// delegates to Run for every non-server subcommand.
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
	switch args[0] {
	case "version":
		return runVersion(stdout)
	default:
		fmt.Fprintf(stderr, "blind-llm-eyes: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// runVersion prints "blind-llm-eyes <Version> (go <runtime>)".
func runVersion(stdout io.Writer) int {
	fmt.Fprintf(stdout, "blind-llm-eyes %s (go %s)\n", buildinfo.Version, runtime.Version())
	return 0
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
	fmt.Fprintln(w, "  version      Print version information")
}
