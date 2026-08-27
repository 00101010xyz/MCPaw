// Command mcpaw is the MCPaw binary: the composition root described in
// docs/ARCHITECTURE.md §3.
//
// This file is deliberately the only place that constructs concrete adapters
// and wires them together — every other package receives its collaborators as
// interfaces. That is what makes the dependency graph readable in one file and
// every component replaceable without touching the rest of the codebase.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via:
//
//	go build -ldflags="-X main.version=1.2.3"
var version = "dev"

func main() {
	// The subcommand is the first non-flag argument; "serve" is the default so
	// that plain `mcpaw` (as a container ENTRYPOINT) does the useful thing.
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "keygen":
		err = runKeygen(args)
	case "healthcheck":
		err = runHealthcheck(args)
	case "-h", "--help", "help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "mcpaw: unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpaw: %v\n", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func printUsage() {
	fmt.Fprint(os.Stderr, `mcpaw — MCP servers for ordinary HTTP APIs

Usage:
  mcpaw [serve]        Run the platform (default command).
  mcpaw keygen         Generate a master key and print it, for MCPAW_MASTER_KEY.
  mcpaw healthcheck    Query /healthz on the locally running instance; exits
                        non-zero on failure. Intended for a container HEALTHCHECK.

Configuration is read from the environment; see README.md for the full list of
MCPAW_* variables.
`)
}
