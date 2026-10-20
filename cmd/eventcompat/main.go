// Command eventcompat checks that an AsyncAPI contract changes each event
// version only additively (ADR-0041): given the base contract (main's) and
// the head, it lists every removed event type or field, changed type and
// newly required field, which need a .vN+1 alongside instead. CI's
// breaking-change step and scripts/ci-local.sh run it beside oasdiff.
//
// Usage: eventcompat <base.yaml> <head.yaml>. It exits 0 when the change
// is additive, 1 when it breaks a consumer, 2 when it cannot check.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/mustie7iy/agentic-erp/api"
)

var exit = os.Exit

func main() { exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: eventcompat <base.yaml> <head.yaml>") // best effort: the exit code reports it
		return 2
	}
	docs := make([][]byte, 2)
	for i, path := range args {
		b, err := os.ReadFile(path) // #nosec G304 G703 -- CI names the two spec files on the command line
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "eventcompat:", err) // best effort: the exit code reports it
			return 2
		}
		docs[i] = b
	}
	changes, err := api.EventChanges(docs[0], docs[1])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "eventcompat:", err) // best effort: the exit code reports it
		return 2
	}
	for _, c := range changes {
		_, _ = fmt.Fprintln(stdout, c) // best effort: the exit code reports the failure
	}
	if len(changes) > 0 {
		_, _ = fmt.Fprintf(stderr, "eventcompat: %d breaking event changes; publish the event as .vN+1 alongside instead (ADR-0041)\n", len(changes))
		return 1
	}
	return 0
}
