package config

import (
	"os"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// Arbitrary YAML never panics the parser, validator or diff.
func FuzzParseValidateDiff(f *testing.F) {
	valid, _ := os.ReadFile("testdata/valid.yaml")
	f.Add(valid)
	f.Add([]byte("version: 1\ncompanies: [{code: X}]\n"))
	f.Add([]byte("purchasing: {requisition_approval: [{steps: [{role: a}]}]}\n"))
	f.Add(append(valid, "extensions: {supplier_invoice: {version: 1, type: object, additionalProperties: false, properties: {a: {type: string, x-pii: none}}}}\n"...))
	base, _ := Parse(valid)
	f.Fuzz(func(t *testing.T, doc []byte) {
		c, err := Parse(doc)
		if err != nil {
			return
		}
		Validate(c, currency.Seeded())
		if _, err := Diff(base, c); err != nil {
			t.Fatal(err)
		}
		ExtensionTransitions(base, c)
		c.RoleNames()
	})
}
