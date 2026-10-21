package datasets

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestV1(t *testing.T) {
	sets, err := V1()
	if err != nil {
		t.Fatalf("V1: %v", err)
	}
	gl := sets["gl_lines"]
	if gl == nil || gl.Kind != "fact" {
		t.Fatalf("gl_lines: got %+v, want a fact dataset", gl)
	}
	if c := gl.Column("amount_minor"); c == nil || c.Role != "amount" {
		t.Errorf("amount_minor: got %+v, want the amount column", c)
	}
	if c := gl.Column("iban"); c != nil {
		t.Errorf("iban: got %+v, want no such column", c)
	}
	if got := len(gl.WithRole("tenant")); got != 1 {
		t.Errorf("tenant columns: got %d, want 1", got)
	}
}

func TestParseRefuses(t *testing.T) {
	if _, err := parse(fstest.MapFS{}, "["); err == nil {
		t.Error("bad pattern: got nil error, want one")
	}
	for name, tc := range map[string]struct {
		fs   fstest.MapFS
		want string
	}{
		"empty":         {fstest.MapFS{}, "no schema file"},
		"unknown field": {fstest.MapFS{"v/a.yaml": {Data: []byte("dataset: a\nextra: 1\n")}}, "field extra not found"},
		"misnamed":      {fstest.MapFS{"v/a.yaml": {Data: []byte("dataset: b\n")}}, `want "a"`},
		"unreadable":    {fstest.MapFS{"v/a.yaml": {Mode: 0o200 | 1<<31}}, "datasets:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(tc.fs, "v")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parse: got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
