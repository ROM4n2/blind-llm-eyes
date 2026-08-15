package cli

import (
	"fmt"
	"io"
)

// runCache implements the `cache` subcommand: manage the persistent cache.
// Subcommands: stats / list / clear / path. They are implemented in later
// tasks; for now every subcommand returns "not implemented yet".
func runCache(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCacheUsage(stderr)
		return 2
	}
	switch args[0] {
	case "stats", "list", "clear", "path":
		fmt.Fprintf(stderr, "cache %s: not implemented yet\n", args[0])
		return 2
	default:
		fmt.Fprintf(stderr, "cache: unknown subcommand %q\n", args[0])
		printCacheUsage(stderr)
		return 2
	}
}

func printCacheUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: blind-llm-eyes cache <subcommand>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  stats   Show cache statistics (entries, size, oldest/newest access)")
	fmt.Fprintln(w, "  list    List cache entries (hash prefix + description preview)")
	fmt.Fprintln(w, "  clear   Delete all cache entries")
	fmt.Fprintln(w, "  path    Show the cache database path and type")
}
