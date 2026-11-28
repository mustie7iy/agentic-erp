package integration

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
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
	_, more := materialsOf(x, target{units: map[string]string{"KG": "mass"}}, plants)
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
		{"2.5", 0, false}, {"", 0, false}, {"x", 0, false},
		{"4/2", 0, false}, {"1e3", 0, false}, {"99999999999999999999", 0, false}}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, ok := factor(c.in); got != c.want || ok != c.ok {
				t.Errorf("factor(%q): got %d %t, want %d %t", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestMaterialTypeAndGroupConfigured: once the configuration lists
// material types and groups, a material whose type or group it does not
// list is held back with the reason, a warning, and one it lists loads;
// without lists every platform type and any group of materials' form
// loads (ADR-0082 As built S1a4).
func TestMaterialTypeAndGroupConfigured(t *testing.T) {
	lists := config.Materials{MaterialTypes: []config.MaterialType{{Code: "raw_material", Name: "Raw materials"}},
		MaterialGroups: []config.MaterialGroup{{Code: "STEEL", Name: "Steel"}}}
	x := &sap.Extract{T001W: []sap.T001W{{WERKS: "P1", NAME1: "One", BUKRS: "2000"}},
		MARA: []sap.MARA{{MATNR: "M-1", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"}, {MATNR: "M-2", MTART: "DIEN", MATKL: "STEEL", MEINS: "KG"},
			{MATNR: "M-3", MTART: "ROH", MATKL: "WOOD", MEINS: "KG"}},
		MAKT: []sap.MAKT{{MATNR: "M-1", SPRAS: "E", MAKTX: "Sheet"}, {MATNR: "M-2", SPRAS: "E", MAKTX: "Cutting"}, {MATNR: "M-3", SPRAS: "E", MAKTX: "Plank"}},
		MARC: []sap.MARC{{MATNR: "M-1", WERKS: "P1"}, {MATNR: "M-2", WERKS: "P1"}, {MATNR: "M-3", WERKS: "P1"}}}
	plants, _ := plantsOf(x)
	for _, c := range []struct {
		name     string
		lists    config.Materials
		loaded   string
		heldBack []string
	}{
		{"configured", lists, "M-1", []string{
			`warning MARA M-2: held back: material type "DIEN" (service) is not one of the configuration's material types`,
			`warning MARA M-3: held back: material group "WOOD" is not one of the configuration's material groups`}},
		{"not configured", config.Materials{}, "M-1 M-2 M-3", nil},
		{"loaded before the lists", lists, "M-1 M-2 M-3", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			tg := target{units: map[string]string{"KG": "mass"}, materials: c.lists}
			if c.name == "loaded before the lists" {
				tg.migrated = map[string]bool{"M-2": true, "M-3": true}
			}
			mats, issues := materialsOf(x, tg, plants)
			var loaded, got []string
			for _, m := range mats {
				loaded = append(loaded, m.number)
			}
			for _, i := range issues {
				got = append(got, i.Severity+" "+i.Table+" "+i.Key+": "+i.Message)
			}
			if strings.Join(loaded, " ") != c.loaded {
				t.Errorf("loaded: got %v, want %s", loaded, c.loaded)
			}
			if !slices.Equal(got, c.heldBack) {
				t.Errorf("issues: got %v, want %v", got, c.heldBack)
			}
		})
	}
}
