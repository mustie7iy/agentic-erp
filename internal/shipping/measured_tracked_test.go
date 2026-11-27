package shipping_test

import (
	"testing"
)

// TestMeasuredBatchIssued (batch 97S integration: S1a3 with S2f2 and
// S2j2): a batch-managed material whose base unit carries a GTIN and its
// weights is found by its GTIN, ordered, confirmed, delivered and issued
// from its batch as any batch-managed material is: nothing in the order,
// the delivery or the goods issue reads the measures yet (the remainder
// of the material master's split leaf).
func TestMeasuredBatchIssued(t *testing.T) {
	e := setup(t)
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Thread sealant", "base_unit": "EA",
		"material_type": "trading_good", "material_group": "PARTS", "confirm_duplicate": true, "batch_managed": true,
		"base_unit_measures": map[string]any{"gtin": "4006381333931", "gross_weight": "0.25", "net_weight": "0.2", "weight_unit": "KG"},
		"plants":             []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	sealant := m["id"].(string)
	e.value(t, sealant, "3000", "moving_average", "2.00")
	page, _ := e.Must(200, "GET", "/v1/materials?gtin=4006381333931", "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != sealant {
		t.Fatalf("by its GTIN: got %v, want the sealant alone", items)
	}
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{
		e.stockItem(sealant, "10", map[string]any{"batch_id": e.newBatch(t, sealant, "L1")})}}), "")
	order := e.askedFor(t, "", e.ofMaterial(sealant, "4 EA"))
	if got, want := confirmedOf(t, e.confirmedOn(t, order)), "4 EA"; got != want {
		t.Fatalf("confirmed: got %s, want %s", got, want)
	}
	id, tag := e.picked(t, order.String(), "1 4 EA")
	e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 4 EA", "L1")), tag)
	if got, want := e.stockByBatch(t, sealant), "L1 6"; got != want {
		t.Errorf("the batch after the goods issue: got %q, want %q", got, want)
	}
	got, _ := e.Must(200, "GET", "/v1/materials/"+sealant, "", "")
	if gtin := got["base_unit_measures"].(map[string]any)["gtin"]; gtin != "4006381333931" {
		t.Errorf("the GTIN after the goods issue: got %v, want it unchanged", gtin)
	}
}
