// Command enumcompat checks that a contract withdraws no value of an open
// enumeration (x-extensible-enum): given the accepted breaking changes
// (api/breaking-accepted.txt), the base contract (main's) and the head, it
// lists every value the base lists and the head no longer does, unless
// the accepted list carries that line under its reason (06 §6.5,
// ADR-0041). oasdiff does not read the extension, so CI's breaking-change
// step and scripts/ci-local.sh run this beside it.
//
// Usage: enumcompat <accepted.txt> <base.yaml> <head.yaml>. It exits 0
// when no value was removed without being accepted, 1 when one was, 2
// when it cannot check.
package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/api"
)

var exit = os.Exit

func main() { exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		_, _ = fmt.Fprintln(stderr, "usage: enumcompat <accepted.txt> <base.yaml> <head.yaml>") // best effort: the exit code reports it
		return 2
	}
	files := make([][]byte, 3)
	for i, path := range args {
		b, err := os.ReadFile(path) // #nosec G304 G703 -- CI names the three files on the command line
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "enumcompat:", err) // best effort: the exit code reports it
			return 2
		}
		files[i] = b
	}
	removed, err := api.EnumRemovals(files[1], files[2])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "enumcompat:", err) // best effort: the exit code reports it
		return 2
	}
	var accepted []string
	for line := range strings.Lines(string(files[0])) {
		accepted = append(accepted, strings.TrimSpace(line))
	}
	refused := 0
	for _, r := range removed {
		if slices.Contains(accepted, r) {
			continue
		}
		refused++
		_, _ = fmt.Fprintln(stdout, r) // best effort: the exit code reports the failure
	}
	if refused > 0 {
		_, _ = fmt.Fprintf(stderr, "enumcompat: values removed from open enumerations: %d; keep a documented /v1 value, or list its line above in %s under its reason (ADR-0041)\n", refused, args[0])
		return 1
	}
	return 0
}
