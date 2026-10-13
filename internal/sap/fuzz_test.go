package sap

import (
	"strings"
	"testing"
)

// FuzzODataString checks that any value becomes one OData string literal:
// quoted, with no single quote inside that is not doubled, and reading it
// back (unquoting, collapsing ” to ') gives the value again.
func FuzzODataString(f *testing.F) {
	for _, s := range []string{"", "1000", "'", "''", "10' or CompanyCode ne '", "a'b'c'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		lit := odataString(s)
		if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
			t.Fatalf("got %q, want a quoted literal", lit)
		}
		inner := lit[1 : len(lit)-1]
		if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
			t.Fatalf("got %q, want every quote inside doubled", lit)
		}
		if got := strings.ReplaceAll(inner, "''", "'"); got != s {
			t.Fatalf("got %q back from %q, want %q", got, lit, s)
		}
	})
}

func FuzzODataValue(f *testing.F) {
	for _, s := range []string{"/Date(1767225600000)/", "/Date(-1)/", "/Date(abc)/", "/Date(1+0100)/", "plain"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) { odataValue(s) })
}
