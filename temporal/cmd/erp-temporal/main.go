// Command erp-temporal is a cell's workflow engine (ADR-0012, ADR-0035):
// Temporal's server with the platform's authorization, which admits the
// cell's server by its certificate's SPIFFE identity and nothing else
// (ADR-0033, spiffeauth), at TLS 1.3. It is built, signed and attested like
// the platform's own binaries (ADR-0032).
//
//	erp-temporal serve          runs the engine
//	erp-temporal setup-schema   creates its login, databases and schema
//	erp-temporal check-config   loads a configuration as serve would, and
//	                            refuses what serve refuses (the chart's checks)
//
// It lives in a module of its own (ADR-0035): Temporal's server brings
// about four hundred modules the platform does not otherwise need, and
// builds only with OpenTelemetry versions older than the platform's.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// exit is os.Exit, replaced by tests.
var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	exit(code)
}

// cli runs a command and returns the process's exit code: 2 for bad usage
// or settings, 1 for a failure while running, 0 when done.
func cli(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: erp-temporal serve | setup-schema | check-config")
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(ctx, getenv, stderr)
	case "setup-schema":
		return schemaCmd(ctx, getenv, stderr)
	case "check-config":
		return checkCmd(getenv, stderr)
	}
	fmt.Fprintf(stderr, "erp-temporal: unknown command %q (serve runs the engine, setup-schema prepares its database, check-config checks a configuration)\n", args[0])
	return 2
}
