package materials_test

import (
	"testing"
)

// TestDescriptionsAndMeasures (batch 97S integration: S1a3 with S1a2):
// descriptions by language and the units' measures are both fields of a
// material's change: a change naming the descriptions leaves the base
// unit's and the box's measures as they are, and one naming the measures
// leaves the descriptions, each change recording only its own field.
func TestDescriptionsAndMeasures(t *testing.T) {
	e := setup(t)
	id := e.active(t, map[string]any{"descriptions": further(arabic, "Équerre"), "base_unit_measures": eachMeasures(),
		"alternative_units": box(boxMeasures(boxGTIN))})["id"].(string)
	t.Run("descriptions leave the measures", func(t *testing.T) {
		c := e.change(t, id, mustJSON(t, map[string]any{"descriptions": []any{map[string]any{"language": "de", "description": "Winkel M10"}}}))
		fields(t, e.audits(t, c["id"].(string), "materials.change.request")[0], map[string]any{"fields": "[descriptions]"})
		e.Approve(t, c["id"].(string))
		got, _ := e.Must(200, "GET", path+"/"+id, "", "")
		if g, want := descriptions(t, got), "en Machined bracket M10; de Winkel M10"; g != want {
			t.Errorf("descriptions: got %q, want %q", g, want)
		}
		if g := measured(got["base_unit_measures"]); g != eachRead {
			t.Errorf("base unit's measures: got %q, want %q", g, eachRead)
		}
		if g := measured(rows(t, got, "alternative_units")[0]["measures"]); g != boxRead {
			t.Errorf("box's measures: got %q, want %q", g, boxRead)
		}
	})
	t.Run("measures leave the descriptions", func(t *testing.T) {
		c := e.change(t, id, mustJSON(t, map[string]any{"base_unit_measures": map[string]any{"gross_weight": "0.13", "weight_unit": "KG"}}))
		fields(t, e.audits(t, c["id"].(string), "materials.change.request")[0], map[string]any{"fields": "[base_unit_measures]"})
		e.Approve(t, c["id"].(string))
		got, _ := e.Must(200, "GET", path+"/"+id, "", "")
		if g, want := measured(got["base_unit_measures"]), "gross_weight=0.13 weight_unit=KG weight_unit_name=Kilogram "; g != want {
			t.Errorf("base unit's measures: got %q, want %q", g, want)
		}
		if g, want := descriptions(t, got), "en Machined bracket M10; de Winkel M10"; g != want {
			t.Errorf("descriptions: got %q, want %q", g, want)
		}
		if g := measured(rows(t, got, "alternative_units")[0]["measures"]); g != boxRead {
			t.Errorf("box's measures: got %q, want %q", g, boxRead)
		}
	})
}
