package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

const withRules = `rules:
  supplier_invoice:
    project_code:
      expression: ext.project.startsWith("P-") && ext.count.orValue(0) < 10
      message: projects are coded P-...
      cases:
        - row: &inv {legal_entity_id: le, supplier_id: s, supplier_reference: r, invoice_date: "2026-10-20",
                     gross_amount: {amount: "1.00", currency: EUR}, tax_amount: {amount: "0.00", currency: EUR}}
          extensions: {project: P-1, count: 2}
          valid: true
        - row: *inv
          extensions: {project: X-1}
          valid: false
`

// Rules are compiled against the standard fields and the document's own
// custom field schema; their changes are tier 3.
func TestRulesValidate(t *testing.T) {
	c, err := Parse(append(append(fixtureYAML(t), withExtensions...), withRules...))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Errorf("got findings %v, want none", fs)
	}
	changes, _ := Diff(withSchema(t), c) // its error is always nil (Diff's contract)
	for _, ch := range changes {
		if ch.Tier != TierPostingRelated {
			t.Errorf("%s: got tier %d, want %d", ch.Path, ch.Tier, TierPostingRelated)
		}
	}
	if len(changes) == 0 {
		t.Error("got no changes, want the added rule")
	}
	r := c.Rules["supplier_invoice"]["project_code"]
	r.Expression = "ext.nope"
	c.Rules["supplier_invoice"]["project_code"] = r
	c.Rules["journal_entry"] = nil
	fs := Validate(c, currency.Seeded())
	for _, want := range []string{"rules.supplier_invoice.project_code.expression", "rules.journal_entry"} {
		if !hasFinding(fs, want) {
			t.Errorf("got %v, want a finding at %s", fs, want)
		}
	}
}

func hasFinding(fs []Finding, path string) bool {
	for _, f := range fs {
		if f.Path == path && f.Severity == SevError {
			return true
		}
	}
	return false
}
