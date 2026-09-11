// Command infermesh is the unified InferMesh binary.
//
// Subcommands:
//
//	infermesh router [flags]  run the router (with --worker, an in-process
//	                          worker is registered against itself; dev mode only)
//	infermesh worker [flags]  run a standalone worker
//
// A bare invocation or an unknown subcommand prints usage and exits 2.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "router":
		os.Exit(runRouter(os.Args[2:]))
	case "worker":
		os.Exit(runWorker(os.Args[2:]))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "infermesh: unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

// usage prints the top-level help text.
func usage(w io.Writer) {
	// os.Stdout/os.Stderr writes cannot fail in practice; errcheck is silenced
	// with an explicit blank assignment.
	_, _ = fmt.Fprint(w, `Usage: infermesh <subcommand> [flags]

Subcommands:
  router  Run the router. With --worker, an in-process worker is started
          and registered against this router (dev mode only).
  worker  Run a standalone worker.

Run "infermesh <subcommand>" without flags for its flag help.
`)
}
