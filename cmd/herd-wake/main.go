// Command herd-wake is an on-demand lifecycle supervisor for Node.js
// development servers running alongside Laravel Herd.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/michael-hewitt/herd-wake/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the given command-line arguments and returns the process
// exit code. Output is written to stdout, diagnostics to stderr.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "herd-wake %s\n", version.String())
		return 0
	default:
		fmt.Fprintf(stderr, "herd-wake: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: herd-wake <command>

Commands:
  version    Print the herd-wake version
`)
}
