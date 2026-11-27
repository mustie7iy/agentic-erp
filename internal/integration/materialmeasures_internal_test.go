package integration

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/materials"
)

// TestQuantity (S1a3): SAP's quantities are written as the platform keeps
// a measure, zero and empty as none; what is no decimal, has more than
// three decimals or ten whole digits is refused.
func TestQuantity(t *testing.T) {
	for in, want := range map[string]struct {
		out string
		ok  bool
	}{
		"": {"", true}, " ": {"", true}, "0": {"", true}, "0.000": {"", true}, "007.50": {"7.5", true}, "1.2340": {"1.234", true},
		"0.5": {"0.5", true}, "18": {"18", true}, "1000.000": {"1000", true}, "9999999999.999": {"9999999999.999", true},
		"12345678901": {"", false}, "1.2345": {"", false}, "1e3": {"", false}, "-1": {"", false}, "1,5": {"", false}, ".5": {"", false},
	} {
		t.Run(in, func(t *testing.T) {
			got, ok := quantity(in)
			if got != want.out || ok != want.ok {
				t.Errorf("got %q %v, want %q %v", got, ok, want.out, want.ok)
			}
		})
	}
}

// TestNewGTINs (S1a3): the GTINs a run locks are those of the materials
// it numbers, each unit's, the base unit's first.
func TestNewGTINs(t *testing.T) {
	mats := []sapMaterial{
		{number: "M-1", baseMeasures: materials.Measures{GTIN: "036000291452"},
			units: []materials.UnitInput{{Code: "BOX", Measures: materials.Measures{GTIN: "10036000291459"}}, {Code: "T"}}},
		{number: "M-2", baseMeasures: materials.Measures{GTIN: "4006381333931"}},
		{number: "M-3"},
	}
	got := strings.Join(newGTINs(mats, map[string]string{"M-1": "MA1", "M-3": "MA3"}), " ")
	if want := "036000291452 10036000291459"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
