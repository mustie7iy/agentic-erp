package config

import (
	"os"
	"testing"
)

// Arbitrary YAML never panics the parser, validator or diff.
func FuzzParseValidateDiff(f *testing.F) {
	valid, _ := os.ReadFile("testdata/valid.yaml")
	f.Add(valid)
	f.Add([]byte("version: 1\ncompanies: [{code: X}]\n"))
	f.Add([]byte("users: [{id: a, roles: [b]}]\nsod_conflicts: [[a, b]]\n"))
	base, _ := Parse(valid)
	f.Fuzz(func(t *testing.T, doc []byte) {
		c, err := Parse(doc)
		if err != nil {
			return
		}
		Validate(c)
		if _, err := Diff(base, c); err != nil {
			t.Fatal(err)
		}
		for _, co := range c.Companies {
			c.Settings(co.Code)
		}
	})
}
