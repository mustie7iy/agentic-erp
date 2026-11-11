package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun(t *testing.T) {
	const doc = "components:\n  schemas:\n    Reason:\n      type: string\n      x-extensible-enum: [blocked, method_not_allowed]\n"
	const removal = "x-extensible-enum at `#/components/schemas/Reason` removed the value `method_not_allowed`\n"
	base := write(t, "base.yaml", doc)
	removed := write(t, "removed.yaml", strings.Replace(doc, ", method_not_allowed", "", 1))
	bad := write(t, "bad.yaml", "components: [")
	none := write(t, "none.txt", "# T27 (ADR-0012 accepted): another change\nPOST /v1/x removed the success response\n")
	accepted := write(t, "accepted.txt", "# T45c (product owner accepted): nothing writes the value\n  "+removal)
	for _, c := range []struct {
		name   string
		args   []string
		code   int
		stdout string
	}{
		{"unchanged", []string{none, base, base}, 0, ""},
		{"a value removed", []string{none, base, removed}, 1, removal},
		{"a removal the list accepts", []string{accepted, base, removed}, 0, ""},
		{"a value added", []string{none, removed, base}, 0, ""},
		{"usage", []string{base, removed}, 2, ""},
		{"missing file", []string{filepath.Join(t.TempDir(), "none.txt"), base, removed}, 2, ""},
		{"unreadable contract", []string{none, base, bad}, 2, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			if got := run(c.args, &stdout, &stderr); got != c.code {
				t.Errorf("exit code: got %d, want %d (stderr %q)", got, c.code, stderr.String())
			}
			if stdout.String() != c.stdout {
				t.Errorf("stdout: got %q, want %q", stdout.String(), c.stdout)
			}
		})
	}
}

func TestMainExitsWithTheCode(t *testing.T) {
	code := -1
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = os.Exit })
	args := os.Args
	os.Args = []string{"enumcompat"}
	t.Cleanup(func() { os.Args = args })
	main()
	if code != 2 {
		t.Errorf("exit code: got %d, want 2 for missing arguments", code)
	}
}
