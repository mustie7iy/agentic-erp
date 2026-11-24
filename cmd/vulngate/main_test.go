package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The scan's messages, as govulncheck v1.9.0 writes them (trimmed from
// temporal/'s scan of 2026-11-23).
const (
	config = `{"config":{"scanner_name":"govulncheck","scanner_version":"v1.9.0","scan_level":"symbol","scan_mode":"source"}}`
	osv    = `{"osv":{"id":"GO-2026-6974","summary":"AttributeValueLengthLimit bypass via U+FFFD runes in go.opentelemetry.io/otel/sdk"}}`
	// The module- and package-level findings of a called vulnerability, and
	// a vulnerability only imported, which the gate does not fail on.
	listed = `{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel/sdk","version":"v1.45.0"}]}}
{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel/sdk","version":"v1.45.0","package":"go.opentelemetry.io/otel/sdk/internal/attrnorm"}]}}
{"finding":{"osv":"GO-2026-6975","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel","version":"v1.45.0","package":"go.opentelemetry.io/otel/baggage"}]}}
{"finding":{"osv":"GO-2026-6975","fixed_version":"v1.47.0","trace":[]}}`
	// Called: twice in otel/sdk (reported once) and once in otel/trace.
	calledSDK = `{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel/sdk","version":"v1.45.0","package":"go.opentelemetry.io/otel/sdk/internal/attrnorm","function":"Truncate","position":{"filename":"internal/attrnorm/truncate.go","line":25,"column":6}},{"module":"m","package":"m/cmd/erp-temporal","function":"init","position":{"filename":"cmd/erp-temporal/namespace.go","line":15,"column":2}}]}}
{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel/sdk","version":"v1.45.0","package":"go.opentelemetry.io/otel/sdk/internal/attrnorm","function":"Truncate"},{"module":"m","package":"m/cmd/erp-temporal","function":"main"}]}}`
	calledTrace = `{"finding":{"osv":"GO-2026-6974","fixed_version":"v1.47.0","trace":[{"module":"go.opentelemetry.io/otel/trace","version":"v1.45.0","package":"go.opentelemetry.io/otel/trace","function":"Start","receiver":"autoTracer"},{"module":"m","package":"m/spiffeauth","function":"init"}]}}`
)

const refusedOutput = `GO-2026-6974: AttributeValueLengthLimit bypass via U+FFFD runes in go.opentelemetry.io/otel/sdk
  More info: https://pkg.go.dev/vuln/GO-2026-6974
  go.opentelemetry.io/otel/sdk@v1.45.0, fixed in v1.47.0: cmd/erp-temporal/namespace.go:15:2 reaches go.opentelemetry.io/otel/sdk/internal/attrnorm.Truncate
  go.opentelemetry.io/otel/trace@v1.45.0, fixed in v1.47.0: m/spiffeauth.init reaches go.opentelemetry.io/otel/trace.autoTracer.Start
`

func write(t *testing.T, dir, name, src string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func entry(id, module, expires string) string {
	return "  - id: " + id + "\n    module: " + module + "\n    expires: \"" + expires + "\"\n    reason: no fixed release builds\n"
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module m\n")
	write(t, root, "temporal/go.mod", "module m/temporal\n")
	file := func(name, src string) string { return write(t, root, name, src) }
	accepts := file("accepts.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-07"))
	elsewhere := file("elsewhere.yaml", "exceptions:\n"+entry("GO-2026-6974", ".", "2026-12-07"))
	expired := file("expired.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-11-22"))
	unused := file("unused.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-07")+entry("GO-2026-0001", "temporal", "2026-12-07"))
	empty := file("empty.yaml", "")
	scan := strings.Join([]string{config, osv, listed, calledSDK, calledTrace}, "\n")
	clean := strings.Join([]string{config, osv, listed}, "\n")
	now = func() time.Time { return time.Date(2026, 11, 23, 23, 0, 0, 0, time.FixedZone("UAE", 4*3600)) }
	t.Cleanup(func() { now = time.Now })
	for _, c := range []struct {
		name, file, module, stdin string
		code                      int
		stdout                    string
	}{
		{"nothing called", empty, "temporal", clean, 0, "vulngate: temporal: 0 called vulnerabilities, 0 accepted by an exception, 0 not\n"},
		{"called, no exception", empty, "temporal", scan, 1, refusedOutput +
			"vulngate: temporal: 1 called vulnerabilities, 0 accepted by an exception, 1 not\n"},
		{"called, accepted on the day it was published", accepts, "temporal", scan, 0,
			"GO-2026-6974: AttributeValueLengthLimit bypass via U+FFFD runes in go.opentelemetry.io/otel/sdk\n" +
				"  accepted in module temporal until 2026-12-07 (" + accepts + "): no fixed release builds\n" +
				"vulngate: temporal: 1 called vulnerabilities, 1 accepted by an exception, 0 not\n"},
		{"an exception for another module accepts nothing here", elsewhere, "temporal", scan, 1, refusedOutput +
			"vulngate: temporal: 1 called vulnerabilities, 0 accepted by an exception, 1 not\n"},
		{"an exception for another module is not this run's to call unused", elsewhere, "temporal", clean, 0,
			"vulngate: temporal: 0 called vulnerabilities, 0 accepted by an exception, 0 not\n"},
		{"called, exception expired", expired, "temporal", scan, 1, refusedOutput +
			"  its exception in " + expired + " expired on 2026-11-22: fix it, or give a new reason and date\n" +
			"vulngate: temporal: 1 called vulnerabilities, 0 accepted by an exception, 1 not\n"},
		{"an exception no called vulnerability matches", unused, "temporal", scan, 1,
			"GO-2026-6974: AttributeValueLengthLimit bypass via U+FFFD runes in go.opentelemetry.io/otel/sdk\n" +
				"  accepted in module temporal until 2026-12-07 (" + unused + "): no fixed release builds\n" +
				"GO-2026-0001: the exception in " + unused + " for module temporal (until 2026-12-07) matches no called vulnerability: remove it\n" +
				"vulngate: temporal: 1 called vulnerabilities, 1 accepted by an exception, 0 not\n"},
		{"the fixed module leaves its exception unused", accepts, "temporal", clean, 1,
			"GO-2026-6974: the exception in " + accepts + " for module temporal (until 2026-12-07) matches no called vulnerability: remove it\n" +
				"vulngate: temporal: 0 called vulnerabilities, 0 accepted by an exception, 0 not\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if got := run([]string{"-root", root, "-module", c.module, c.file}, strings.NewReader(c.stdin), &stdout, &stderr); got != c.code {
				t.Errorf("exit code: got %d, want %d (stderr %q)", got, c.code, stderr.String())
			}
			if stdout.String() != c.stdout {
				t.Errorf("stdout: got %q, want %q", stdout.String(), c.stdout)
			}
		})
	}
}

func TestExpiresAtTheEndOfItsDayInUTC(t *testing.T) {
	root := t.TempDir()
	write(t, root, "temporal/go.mod", "module m/temporal\n")
	accepts := write(t, root, "x.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-07"))
	scan := strings.Join([]string{config, osv, calledSDK}, "\n")
	t.Cleanup(func() { now = time.Now })
	for _, c := range []struct {
		name string
		at   time.Time
		code int
	}{
		{"its last day", time.Date(2026, 12, 7, 23, 59, 59, 0, time.UTC), 0},
		{"the next day", time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC), 1},
		{"the next day in UTC, still its day in New York", time.Date(2026, 12, 7, 20, 0, 0, 0, time.FixedZone("EST", -5*3600)), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			now = func() time.Time { return c.at }
			var stdout, stderr strings.Builder
			if got := run([]string{"-root", root, "-module", "temporal", accepts}, strings.NewReader(scan), &stdout, &stderr); got != c.code {
				t.Errorf("exit code: got %d, want %d (stdout %q)", got, c.code, stdout.String())
			}
		})
	}
}

func TestCannotCheck(t *testing.T) {
	root := t.TempDir()
	write(t, root, "temporal/go.mod", "module m/temporal\n")
	file := func(name, src string) string { return write(t, root, name, src) }
	empty := file("empty.yaml", "")
	ok := strings.Join([]string{config, osv, calledSDK}, "\n")
	for _, c := range []struct {
		name   string
		args   []string
		stdin  string
		stderr string
	}{
		{"no file", []string{"-root", root, "-module", "temporal"}, ok, "usage:"},
		{"no root", []string{"-module", "temporal", empty}, ok, "usage:"},
		{"no module", []string{"-root", root, empty}, ok, "usage:"},
		{"unknown flag", []string{"-today", "2026-12-08", empty}, ok, "usage:"},
		{"missing file", []string{"-root", root, "-module", "temporal", filepath.Join(root, "none.yaml")}, ok, "exceptions: open"},
		{"not YAML", []string{"-root", root, "-module", "temporal", file("bad.yaml", "exceptions: [")}, ok, "bad.yaml: yaml:"},
		{"an unknown field", []string{"-root", root, "-module", "temporal", file("field.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-07")+"    until: never\n")}, ok, "field until not found"},
		{"no reason", []string{"-root", root, "-module", "temporal", file("reason.yaml", "exceptions:\n  - {id: GO-2026-6974, module: temporal, expires: \"2026-12-07\"}\n")}, ok, "exception 1: id, module, expires and reason are all required"},
		{"not a date", []string{"-root", root, "-module", "temporal", file("date.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-7"))}, ok, `expires "2026-12-7" is not a date`},
		{"not a module", []string{"-root", root, "-module", "temporal", file("module.yaml", "exceptions:\n"+entry("GO-2026-6974", "tempral", "2026-12-07"))}, ok, `module "tempral" is not a Go module of the repository`},
		{"not a clean directory", []string{"-root", root, "-module", "temporal", file("clean.yaml", "exceptions:\n"+entry("GO-2026-6974", "./temporal", "2026-12-07"))}, ok, `module "./temporal" is not a clean directory (write "temporal")`},
		{"listed twice", []string{"-root", root, "-module", "temporal", file("twice.yaml", "exceptions:\n"+entry("GO-2026-6974", "temporal", "2026-12-07")+entry("GO-2026-6974", "temporal", "2026-12-01"))}, ok, "GO-2026-6974 is listed twice for module temporal"},
		{"not JSON", []string{"-root", root, "-module", "temporal", empty}, "=== Symbol Results ===", "temporal: reading govulncheck's JSON"},
		{"no config", []string{"-root", root, "-module", "temporal", empty}, osv, `not a symbol-level scan (scan_level "")`},
		{"a package-level scan", []string{"-root", root, "-module", "temporal", empty}, strings.Replace(ok, `"symbol"`, `"package"`, 1), `scan_level "package"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if got := run(c.args, strings.NewReader(c.stdin), &stdout, &stderr); got != 2 {
				t.Errorf("exit code: got %d, want 2 (stdout %q)", got, stdout.String())
			}
			if !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("stderr: got %q, want it to contain %q", stderr.String(), c.stderr)
			}
		})
	}
}

func TestMainExitsWithTheCode(t *testing.T) {
	code := -1
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = os.Exit })
	args := os.Args
	os.Args = []string{"vulngate"}
	t.Cleanup(func() { os.Args = args })
	main()
	if code != 2 {
		t.Errorf("exit code: got %d, want 2 for missing arguments", code)
	}
}
