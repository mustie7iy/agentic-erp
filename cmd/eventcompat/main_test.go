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
	const doc = "components:\n  messages:\n    A:\n      payload:\n        type: object\n        properties:\n          id: {type: string}\n"
	base := write(t, "base.yaml", doc)
	same := write(t, "same.yaml", doc)
	broken := write(t, "broken.yaml", strings.Replace(doc, "type: string", "type: integer", 1))
	bad := write(t, "bad.yaml", "components: [")
	for _, c := range []struct {
		name   string
		args   []string
		code   int
		stdout string
	}{
		{"additive", []string{base, same}, 0, ""},
		{"breaking", []string{base, broken}, 1, "A.id: type changed from string to integer\n"},
		{"usage", []string{base}, 2, ""},
		{"missing file", []string{base, filepath.Join(t.TempDir(), "none.yaml")}, 2, ""},
		{"unreadable contract", []string{base, bad}, 2, ""},
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
	os.Args = []string{"eventcompat"}
	t.Cleanup(func() { os.Args = args })
	main()
	if code != 2 {
		t.Errorf("exit code: got %d, want 2 for missing arguments", code)
	}
}
