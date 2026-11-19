package sap

import "testing"

// TestFilterScopeKeepsTheScopesMaterials: a scope keeps its company codes'
// plants with their storage locations, the materials with plant data at
// one of them with all their descriptions and units, and of their plant
// and storage location data only the scope's (S2o). A material at a
// plant of company 1000 too is in both scopes, each with its own plants.
func TestFilterScopeKeepsTheScopesMaterials(t *testing.T) {
	all := GenerateMock(42, "2026")
	all.T001W = append(all.T001W, T001W{WERKS: "1100", NAME1: "Chicago plant", BWKEY: "1100", BUKRS: "1000"})
	all.T001L = append(all.T001L, T001L{WERKS: "1100", LGORT: "0101", LGOBE: "Main store"})
	steel := mockMaterials[0].mara.MATNR
	all.MARC = append(all.MARC, MARC{MATNR: steel, WERKS: "1100"})
	all.MARD = append(all.MARD, MARD{MATNR: steel, WERKS: "1100", LGORT: "0101"})
	cases := []struct {
		company string
		want    map[string]int
	}{
		{"3000", map[string]int{"T001W": 2, "T001L": 3, "MARA": 6, "MAKT": 7, "MARM": 9, "MARC": 7, "MARD": 7}},
		{"1000", map[string]int{"T001W": 1, "T001L": 1, "MARA": 1, "MAKT": 2, "MARM": 2, "MARC": 1, "MARD": 1}},
		{"2000", map[string]int{"T001W": 0, "T001L": 0, "MARA": 0, "MAKT": 0, "MARM": 0, "MARC": 0, "MARD": 0}},
	}
	for _, c := range cases {
		t.Run(c.company, func(t *testing.T) {
			got := FilterScope(all, Scope{CompanyCodes: []string{c.company}, FiscalYear: "2026"}).Counts()
			for table, want := range c.want {
				if got[table] != want {
					t.Errorf("%s rows: got %d, want %d", table, got[table], want)
				}
			}
		})
	}
}
