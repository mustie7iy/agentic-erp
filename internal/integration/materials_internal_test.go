package integration

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestPlantsOfUnknownPlant: a storage location or plant data of a plant
// the extract does not have is refused. A scope's extract has none
// (sap.FilterScope keeps the rows of its plants), so this reads an
// extract as a source might give it unfiltered.
func TestPlantsOfUnknownPlant(t *testing.T) {
	x := &sap.Extract{T001W: []sap.T001W{{WERKS: "P1", NAME1: "One", BUKRS: "2000"}}, T001L: []sap.T001L{{WERKS: "P9", LGORT: "S9", LGOBE: "Nowhere"}},
		MARA: []sap.MARA{{MATNR: "M-1", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"}}, MARC: []sap.MARC{{MATNR: "M-1", WERKS: "P9"}}}
	plants, issues := plantsOf(x)
	_, more := materialsOf(x, map[string]bool{"KG": true}, plants)
	var got []string
	for _, i := range append(issues, more...) {
		got = append(got, i.Severity+" "+i.Table+" "+i.Key+": "+i.Message)
	}
	for _, want := range []string{"error T001L P9/S9: plant P9 is not in T001W", "error MARC M-1/P9: plant P9 is not in T001W"} {
		if !slices.Contains(got, want) {
			t.Errorf("issues: got %v, want %s", got, want)
		}
	}
}

// TestFactor: a conversion's numerator or denominator is a whole number
// from 1 to 1,000,000, with or without decimals of zero.
func TestFactor(t *testing.T) {
	cases := []struct {
		in   string
		want int32
		ok   bool
	}{{"20", 20, true}, {" 4.000 ", 4, true}, {"1000000", 1_000_000, true}, {"1000001", 0, false}, {"0", 0, false}, {"-2", 0, false},
		{"2.5", 0, false}, {"", 0, false}, {"x", 0, false}}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, ok := factor(c.in); got != c.want || ok != c.ok {
				t.Errorf("factor(%q): got %d %t, want %d %t", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}
