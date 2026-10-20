package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// sample is a partner of a country with a personal custom field.
func sample(n int, country string) Sample {
	return Sample{ID: uuid.New(), Reference: fmt.Sprintf("BP%d", n), Row: partner(country), Extensions: map[string]any{"vat_id": "DE1", "contact": "Ann"}}
}

// simSchema declares vat_id not personal and contact personal.
func simSchema() *extensions.Schema {
	return &extensions.Schema{Version: 1, Type: "object", AdditionalProperties: new(bool), Properties: map[string]extensions.Property{
		"vat_id": {Type: "string", PII: extensions.PIINone}, "contact": {Type: "string", PII: extensions.PIIPersonal}}}
}

func countryRule(refused string) Rule {
	return Rule{Expression: fmt.Sprintf("row.country_code != %q", refused), Message: "no " + refused,
		Cases: []Case{{Row: partner("XX"), Valid: true}, {Row: partner(refused), Valid: false}}}
}

// TestSimulate: per rule, the rows the proposal newly refuses and newly
// passes, with personal custom fields masked; a rule whose outcome does
// not change is not listed; a proposed rule that fails on a row fails the
// replay, a base rule that fails does not. A historical row is counted
// apart, as unaffected, and not judged.
func TestSimulate(t *testing.T) {
	failing := Rule{Expression: `row.roles[1] != "x"`, Message: "m",
		Cases: []Case{{Row: map[string]any{"kind": "k", "name": "n", "country_code": "DE", "roles": []any{"a", "b"}}, Valid: true},
			{Row: map[string]any{"kind": "k", "name": "n", "country_code": "DE", "roles": []any{"a", "x"}}, Valid: false}}}
	base := Active{ConfigVersion: 4, Schema: simSchema(), Rules: map[string]Rule{"same": countryRule("IT"), "changed": countryRule("FR"), "broken": failing}}
	proposed := Active{Schema: simSchema(), Rules: map[string]Rule{"same": countryRule("IT"), "changed": countryRule("DE")}}
	done := sample(4, "DE") // historical: neither refused nor failed, only counted
	done.Historical = true
	samples := []Sample{sample(1, "DE"), sample(2, "FR"), sample(3, "IT"), done}
	got, err := Simulate(context.Background(), "business_partner", base, proposed, samples)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Rows != 4 || got.Historical != 1 || got.Entity != "business_partner" {
		t.Errorf("replay: got passed %t, rows %d, historical %d, entity %s, want true, 4, 1, business_partner", got.Passed, got.Rows, got.Historical, got.Entity)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules listed: got %+v, want only changed", got.Rules)
	}
	r := got.Rules[0]
	if r.Rule != "changed" || r.NewlyRefused != 1 || r.NewlyPassing != 1 || r.Unevaluable != 0 {
		t.Errorf("changed: got %+v, want one newly refused, one newly passing", r)
	}
	if r.RefusedRows[0].Reference != "BP1" || r.PassingRows[0].Reference != "BP2" {
		t.Errorf("rows: got refused %v, passing %v, want BP1, BP2", r.RefusedRows, r.PassingRows)
	}
	if ext := r.RefusedRows[0].Extensions; ext["vat_id"] != "DE1" || ext["contact"] != extensions.Masked {
		t.Errorf("extensions: got %v, want vat_id shown and contact masked", ext)
	}

	proposed.Rules["broken"] = failing
	got, err = Simulate(context.Background(), "business_partner", base, proposed, samples)
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed {
		t.Error("passed: got true with a proposed rule that fails, want false")
	}
	b := got.Rules[0]
	if b.Rule != "broken" || b.Unevaluable != 3 || b.ErrorRows[0].Error != "fails to evaluate on this input" {
		t.Errorf("broken: got %+v, want three rows it cannot evaluate", b)
	}
}

// Examples are capped, counts are not. A row whose custom fields the
// proposed schema does not accept is skipped, not failed: the custom field
// check refuses its next write before any rule reads it; without a schema
// its every custom field is masked.
func TestSimulateBounds(t *testing.T) {
	var samples []Sample
	for i := range examples + 5 {
		samples = append(samples, Sample{ID: uuid.New(), Reference: fmt.Sprint(i), Row: partner("DE")})
	}
	legacy := Sample{ID: uuid.New(), Reference: "LEGACY", Row: partner("DE"), Extensions: map[string]any{"legacy": "x"}}
	withSchema := Active{Schema: simSchema(), Rules: map[string]Rule{"it": countryRule("IT")}}
	got, err := Simulate(context.Background(), "business_partner", withSchema, Active{Rules: map[string]Rule{"de": countryRule("DE")}},
		append(samples, legacy, Sample{ID: uuid.New(), Reference: "UNREAD", Row: partner("DE"), Extensions: map[string]any{"contact": 1}}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || len(got.Rules) != 1 {
		t.Fatalf("replay: got %+v, want passed with one rule listed", got)
	}
	r := got.Rules[0]
	if r.NewlyRefused != examples+5 || len(r.RefusedRows) != examples {
		t.Errorf("refused: got %d with %d examples, want %d with %d", r.NewlyRefused, len(r.RefusedRows), examples+5, examples)
	}
	if got.Skipped != 2 || got.SkippedRows[0].Reference != "LEGACY" || got.SkippedRows[0].Extensions["legacy"] != extensions.Masked {
		t.Errorf("skipped: got %d %+v, want LEGACY and UNREAD with the legacy key masked", got.Skipped, got.SkippedRows)
	}
}

// Rules or a schema that do not compile are an error, base or proposed.
func TestSimulateDoesNotCompile(t *testing.T) {
	bad := Active{Rules: map[string]Rule{"x": {Expression: "row.nope"}}}
	for name, c := range map[string]struct{ base, proposed Active }{
		"base":     {bad, Active{}},
		"proposed": {Active{}, bad},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Simulate(context.Background(), "business_partner", c.base, c.proposed, nil); err == nil || !strings.Contains(err.Error(), "do not compile") {
				t.Errorf("got %v, want the rules not compiling", err)
			}
		})
	}
}

func TestEntities(t *testing.T) {
	if got := strings.Join(Entities(), ","); got != "business_partner,purchase_order,supplier_invoice" {
		t.Errorf("got %s, want the three entities in name order", got)
	}
}

// A failure is reported by its kind, never with the evaluator's text.
func TestFailure(t *testing.T) {
	for msg, want := range map[string]string{
		"evaluation: operation cancelled: actual cost limit exceeded":  "exceeds its cost limit",
		"evaluation: operation interrupted: context deadline exceeded": "exceeds its time limit",
		"evaluation: no such key: Ann Smith":                           "fails to evaluate on this input",
	} {
		t.Run(want, func(t *testing.T) {
			if got := failure(errors.New(msg)); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
