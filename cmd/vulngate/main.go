// Command vulngate is the govulncheck gate (06 §6.5, ADR-0032 As built):
// it reads govulncheck's JSON output for one Go module and fails on every
// vulnerability the module's code calls, as govulncheck's text mode does,
// unless the exceptions file accepts that advisory for that module until a
// date not yet past. An exception is time-boxed, like the image scan's in
// .grype.yaml, but here the expiry is enforced: an expired entry fails the
// gate, and so does an entry this module's scan no longer needs, so stale
// entries are removed rather than left to cover a later finding.
//
// Usage: govulncheck -format json ./... | vulngate -root DIR -module DIR EXCEPTIONS.yaml
// where -module is the module's directory relative to -root ("." for the
// root's). scripts/vulncheck.sh runs it in every module through
// scripts/each-module.sh. It exits 0 when every called vulnerability is
// accepted, 1 when one is not or an exception is expired or unused, 2 when
// it cannot check.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	exit = os.Exit
	// now is the clock the expiries are read against, replaced by tests.
	now = time.Now
)

func main() { exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vulngate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the repository's root")
	module := fs.String("module", "", "the module's directory, relative to the root")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *root == "" || *module == "" {
		_, _ = fmt.Fprintln(stderr, "usage: govulncheck -format json ./... | vulngate -root DIR -module DIR EXCEPTIONS.yaml") // best effort: the exit code reports it
		return 2
	}
	exceptions, err := readExceptions(fs.Arg(0), *root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vulngate:", err) // best effort: the exit code reports it
		return 2
	}
	called, err := readScan(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "vulngate: %s: %v\n", *module, err) // best effort: the exit code reports it
		return 2
	}
	g := gate{module: *module, file: fs.Arg(0), today: now().UTC().Format(time.DateOnly), out: stdout}
	if !g.check(called, exceptions) {
		return 1
	}
	return 0
}

// An exception accepts one advisory in one module until a date.
type exception struct {
	ID      string `yaml:"id"`
	Module  string `yaml:"module"`
	Expires string `yaml:"expires"`
	Reason  string `yaml:"reason"`
}

// readExceptions reads and checks the exceptions file. Every field is
// required, a date is a calendar date, an advisory is listed once per
// module, and the module is one of the repository's, so an entry cannot
// name a directory no run ever checks.
func readExceptions(path, root string) ([]exception, error) {
	f, err := os.Open(path) // #nosec G304 -- the gate's own command line names the file
	if err != nil {
		return nil, fmt.Errorf("exceptions: %w", err)
	}
	defer func() { _ = f.Close() }() // read only: a failed close loses nothing
	var doc struct {
		Exceptions []exception `yaml:"exceptions"`
	}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[[2]string]bool{}
	for i, e := range doc.Exceptions {
		if e.ID == "" || e.Module == "" || e.Expires == "" || e.Reason == "" {
			return nil, fmt.Errorf("%s: exception %d: id, module, expires and reason are all required", path, i+1)
		}
		if _, err := time.Parse(time.DateOnly, e.Expires); err != nil {
			return nil, fmt.Errorf("%s: %s: expires %q is not a date (YYYY-MM-DD)", path, e.ID, e.Expires)
		}
		if e.Module != filepath.Clean(e.Module) {
			return nil, fmt.Errorf("%s: %s: module %q is not a clean directory (write %q): no run would match it", path, e.ID, e.Module, filepath.Clean(e.Module))
		}
		if _, err := os.Stat(filepath.Join(root, e.Module, "go.mod")); err != nil {
			return nil, fmt.Errorf("%s: %s: module %q is not a Go module of the repository: %w", path, e.ID, e.Module, err)
		}
		key := [2]string{e.ID, e.Module}
		if seen[key] {
			return nil, fmt.Errorf("%s: %s is listed twice for module %s", path, e.ID, e.Module)
		}
		seen[key] = true
	}
	return doc.Exceptions, nil
}

// A frame is one step of a finding's call trace; the first is the
// vulnerable symbol, the last the module's own code.
type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
	Position *struct {
		Filename string `json:"filename"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
	} `json:"position"`
}

type finding struct {
	OSV          string  `json:"osv"`
	FixedVersion string  `json:"fixed_version"`
	Trace        []frame `json:"trace"`
}

// A vuln is one advisory the module's code calls, with the first finding
// for each vulnerable module it is found in.
type vuln struct {
	id, summary string
	findings    []finding
}

// readScan reads govulncheck's JSON stream and returns the advisories the
// code calls, in the order govulncheck first reports them. A finding is
// called when its trace starts at a function: govulncheck reports the
// module- and package-level findings too, which its text mode does not fail
// on. A stream without a symbol-level scan's config is refused, so a
// changed flag cannot quietly weaken the gate.
func readScan(r io.Reader) ([]*vuln, error) {
	var (
		level    string
		summary  = map[string]string{}
		byID     = map[string]*vuln{}
		order    []*vuln
		inModule = map[[2]string]bool{}
	)
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Config *struct {
				ScanLevel string `json:"scan_level"`
			} `json:"config"`
			OSV *struct {
				ID      string `json:"id"`
				Summary string `json:"summary"`
			} `json:"osv"`
			Finding *finding `json:"finding"`
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading govulncheck's JSON: %w", err)
		}
		switch {
		case m.Config != nil:
			level = m.Config.ScanLevel
		case m.OSV != nil:
			summary[m.OSV.ID] = m.OSV.Summary
		case m.Finding != nil && len(m.Finding.Trace) > 0 && m.Finding.Trace[0].Function != "":
			f := *m.Finding
			v := byID[f.OSV]
			if v == nil {
				v = &vuln{id: f.OSV}
				byID[f.OSV] = v
				order = append(order, v)
			}
			if key := [2]string{f.OSV, f.Trace[0].Module}; !inModule[key] {
				inModule[key] = true
				v.findings = append(v.findings, f)
			}
		}
	}
	if level != "symbol" {
		return nil, fmt.Errorf("govulncheck's output is not a symbol-level scan (scan_level %q)", level)
	}
	for _, v := range order {
		v.summary = summary[v.id]
	}
	return order, nil
}

type gate struct {
	module, file, today string
	out                 io.Writer
}

// check prints every called advisory, accepted or not, and every expired
// or unused exception of the module, and reports whether the gate passes.
func (g gate) check(called []*vuln, exceptions []exception) bool {
	mine := map[string]exception{}
	for _, e := range exceptions {
		if e.Module == g.module {
			mine[e.ID] = e
		}
	}
	ok, refused, accepted := true, 0, 0
	for _, v := range called {
		e, listed := mine[v.id]
		delete(mine, v.id)
		if listed && e.Expires >= g.today {
			accepted++
			g.printf("%s: %s\n  accepted in module %s until %s (%s): %s\n", v.id, v.summary, g.module, e.Expires, g.file, e.Reason)
			continue
		}
		ok = false
		refused++
		g.printVuln(v)
		if listed {
			g.printf("  its exception in %s expired on %s: fix it, or give a new reason and date\n", g.file, e.Expires)
		}
	}
	for _, e := range exceptions {
		if _, unused := mine[e.ID]; unused && e.Module == g.module {
			ok = false
			g.printf("%s: the exception in %s for module %s (until %s) matches no called vulnerability: remove it\n", e.ID, g.file, g.module, e.Expires)
		}
	}
	g.printf("vulngate: %s: %d called vulnerabilities, %d accepted by an exception, %d not\n", g.module, len(called), accepted, refused)
	return ok
}

func (g gate) printVuln(v *vuln) {
	g.printf("%s: %s\n  More info: https://pkg.go.dev/vuln/%s\n", v.id, v.summary, v.id)
	for _, f := range v.findings {
		sym, entry := f.Trace[0], f.Trace[len(f.Trace)-1]
		g.printf("  %s@%s, fixed in %s: %s reaches %s\n", sym.Module, sym.Version, f.FixedVersion, where(entry), symbol(sym))
	}
}

func (g gate) printf(format string, a ...any) {
	_, _ = fmt.Fprintf(g.out, format, a...) // best effort: the exit code reports the result
}

// where names a frame by its position in the module's code, as
// govulncheck's text mode does, or by its symbol when it has none.
func where(f frame) string {
	if f.Position == nil || f.Position.Filename == "" {
		return symbol(f)
	}
	return fmt.Sprintf("%s:%d:%d", f.Position.Filename, f.Position.Line, f.Position.Column)
}

func symbol(f frame) string {
	name := f.Function
	if f.Receiver != "" {
		name = f.Receiver + "." + name
	}
	return f.Package + "." + name
}
