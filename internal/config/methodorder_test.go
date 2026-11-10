package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// TestMethodsInRunOrder: a payment run tries the company's methods by
// their order, lowest first, and those without one after them as the
// document lists them (ADR-0038; SAP specialist 2026-11-09, A4(c)).
func TestMethodsInRunOrder(t *testing.T) {
	for _, c := range []struct {
		name    string
		methods []PaymentMethod
		want    []string
	}{
		{"none ordered: as listed", []PaymentMethod{{Code: "T"}, {Code: "C"}, {Code: "A"}}, []string{"T", "C", "A"}},
		{"all ordered", []PaymentMethod{{Code: "T", Order: 3}, {Code: "C", Order: 1}, {Code: "A", Order: 2}}, []string{"C", "A", "T"}},
		{"ordered before the others", []PaymentMethod{{Code: "T"}, {Code: "C", Order: 9}, {Code: "A"}, {Code: "B", Order: 4}}, []string{"B", "C", "T", "A"}},
		{"no methods", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			co := Company{PaymentMethods: c.methods}
			var got []string
			for _, m := range co.MethodsInRunOrder() {
				got = append(got, m.Code)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("run order: got %v, want %v", got, c.want)
			}
			if len(c.methods) > 0 && co.PaymentMethods[0].Code != c.methods[0].Code {
				t.Errorf("the document's list: got %s first, want it unchanged", co.PaymentMethods[0].Code)
			}
		})
	}
}

// TestMethodOrderIsAChange: a method's place in the run order decides
// which method pays a supplier, so changing it is a posting-related
// change of its own path, never invisible to the diff, which identifies
// methods by code whatever their position in the list.
func TestMethodOrderIsAChange(t *testing.T) {
	base, changed := fixture(t), fixture(t)
	changed.Companies[0].PaymentMethods[0].Order = 2
	changes, _ := Diff(base, changed)
	if len(changes) != 1 {
		t.Fatalf("changes: got %v, want 1", changes)
	}
	if got := changes[0]; got.Path != "companies[1000].payment_methods[T].order" || got.Tier != TierPostingRelated {
		t.Errorf("order change: got %s %v, want companies[1000].payment_methods[T].order posting-related", got.Path, got.Tier)
	}
	if fs := Validate(changed, currency.Seeded()); HasErrors(fs) {
		t.Errorf("an ordered method: got %v, want it valid", fs)
	}
}
