package integration_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// withMeasures is withMaterials with the steel sheet's measures (S1a3):
// MARA's weights in kilograms written with SAP's zeros, an EAN-13, a
// volume and the base unit's own MARM row's dimensions, and its tonne's
// weight and EAN-14; the shelving's MARA gives an EAN with a wrong check
// digit, a net weight above its gross, a volume in a unit of length and
// its box's dimensions in centimetres, which the platform has not, and
// its box an EAN that the steel sheet's tonne has too.
func withMeasures() *sap.Extract {
	x := withMaterials()
	for i, r := range x.MARA {
		switch r.MATNR {
		case "M-1":
			x.MARA[i].BRGEW, x.MARA[i].NTGEW, x.MARA[i].GEWEI, x.MARA[i].VOLUM, x.MARA[i].VOLEH, x.MARA[i].EAN11 = "1.050", "1.000", "KG", "0.000127", "M3", "4006381333931"
		case "M-2":
			x.MARA[i].BRGEW, x.MARA[i].NTGEW, x.MARA[i].GEWEI, x.MARA[i].VOLUM, x.MARA[i].VOLEH, x.MARA[i].EAN11 = "18", "19", "KG", "0.5", "M", "4006381333932"
		}
	}
	for i, u := range x.MARM {
		switch u.MATNR + "/" + u.MEINH {
		case "M-1/KG":
			x.MARM[i].LAENG, x.MARM[i].BREIT, x.MARM[i].HOEHE, x.MARM[i].MEABM = "1.000", "0.500", "0.002", "M"
		case "M-1/T":
			x.MARM[i].BRGEW, x.MARM[i].GEWEI, x.MARM[i].EAN11 = "1010", "KG", "10036000291459"
		case "M-2/BOX":
			x.MARM[i].LAENG, x.MARM[i].MEABM, x.MARM[i].EAN11, x.MARM[i].GEWEI = "120", "CM", "10036000291459", "KG"
		}
	}
	return x
}

// TestMigratedMeasures (S1a3): the load gives the steel sheet's base unit
// MARA's weights, volume and GTIN with its own MARM row's dimensions,
// zeros SAP writes left out; measures the platform cannot hold are left
// out with a warning and hold no material back; a GTIN two units share
// is left out of both; the reconciliation compares the measures and
// finds no difference.
func TestMigratedMeasures(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMeasures())
	_, got := e.act(t, e.run(t, e.connector(t, "materials.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", got["status"], quality(got))
	}
	q := quality(got)
	for _, want := range []string{
		`warning MARA M-1/KG: the volume is left out: "0.000127" is no quantity above zero of up to ten digits and three decimals`,
		`warning MARA M-2/EA: the net weight 19 KG is more than the gross weight 18; the weights are left out`,
		`warning MARA M-2/EA: the volume is left out: unit "M" is no unit of volume of the platform`,
		`warning MARA M-2/EA: EAN/UPC "4006381333932" is no GTIN (8, 12, 13 or 14 digits ending in their GS1 check digit); it is left out`,
		`warning MARM M-2/BOX: the dimensions are left out: unit "CM" is no unit of length of the platform`,
		`warning MARM M-1/T: EAN/UPC 10036000291459 is another unit's too; it is left out of both`,
		`warning MARM M-2/BOX: EAN/UPC 10036000291459 is another unit's too; it is left out of both`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("quality: got\n%s\nwant it to hold %s", q, want)
		}
	}
	expectFields(t, "loaded", report(got)["loaded"].(map[string]any), map[string]any{"materials": 2.0})
	m, _ := e.Must(200, "GET", "/v1/materials/"+e.materialID(t, "Material M-1"), "", "")
	if got, want := fmt.Sprint(m["base_unit_measures"]), "map[dimension_unit:M dimension_unit_name:Metre gross_weight:1.05 gtin:4006381333931 height:0.002 "+
		"length:1 net_weight:1 weight_unit:KG weight_unit_name:Kilogram width:0.5]"; got != want {
		t.Errorf("the steel sheet's base unit: got %s, want %s", got, want)
	}
	tonne := m["alternative_units"].([]any)[0].(map[string]any)
	if got, want := fmt.Sprint(tonne["measures"]), "map[gross_weight:1010 weight_unit:KG weight_unit_name:Kilogram]"; got != want {
		t.Errorf("the tonne: got %s, want %s", got, want)
	}
	shelving, _ := e.Must(200, "GET", "/v1/materials/"+e.materialID(t, "Material M-2"), "", "")
	if got := fmt.Sprint(shelving["base_unit_measures"]); got != "map[]" {
		t.Errorf("the shelving's base unit: got %s, want no measures", got)
	}
	_, got = e.act(t, got, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	expectFields(t, "materials with differences", checkOf(t, got, "2000", "Materials: materials with differences"), map[string]any{"sap": "0",
		"platform": "0", "pass": true})
}
