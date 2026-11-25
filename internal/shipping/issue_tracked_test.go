package shipping_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Batches and serial numbers in a delivery's goods issue (S2f2, S2g2;
// ADR-0083 As built S2f2), through the API.

// trackedEnv is env with the sealant, managed in batches L1 (10 EA) and
// L2 (5 EA) at S1, the gauge, with a serial number profile (G1, G2 and G3
// at S1), and the valve, managed in batches with a serial number profile
// (batch V1 holding V-1 and V-2 at S1), each valued at a moving price of
// 2.00 EUR.
type trackedEnv struct {
	env
	sealant, gauge, valve string
	l2                    string
}

func setupTracked(t *testing.T) trackedEnv {
	t.Helper()
	e := trackedEnv{env: setup(t)}
	e.sealant = e.trackedMaterial(t, "Thread sealant", true, false)
	e.gauge = e.trackedMaterial(t, "Pressure gauge", false, true)
	e.valve = e.trackedMaterial(t, "Ball valve", true, true)
	l1, v1 := e.newBatch(t, e.sealant, "L1"), e.newBatch(t, e.valve, "V1")
	e.l2 = e.newBatch(t, e.sealant, "L2")
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{
		e.stockItem(e.sealant, "10", map[string]any{"batch_id": l1}), e.stockItem(e.sealant, "5", map[string]any{"batch_id": e.l2}),
		e.stockItem(e.gauge, "3", map[string]any{"serial_numbers": []string{"G1", "G2", "G3"}}),
		e.stockItem(e.valve, "2", map[string]any{"batch_id": v1, "serial_numbers": []string{"V-1", "V-2"}})}}), "")
	return e
}

// trackedMaterial creates an approved trading good of company 2000 kept
// at S1 of P1, managed in batches or with a serial number profile, valued
// at P1, and returns its id.
func (e env) trackedMaterial(t *testing.T, description string, batches, serials bool) string {
	t.Helper()
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": description, "base_unit": "EA",
		"material_type": "trading_good", "material_group": "PARTS", "confirm_duplicate": true, "batch_managed": batches, "serial_managed": serials,
		"plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	id := m["id"].(string)
	e.value(t, id, "3000", "moving_average", "2.00")
	return id
}

// newBatch creates the material's batch, made on 2026-11-01, and returns
// its id.
func (e env) newBatch(t *testing.T, material, number string) string {
	t.Helper()
	b, _ := e.Must(201, "POST", "/v1/batches", mustJSON(t, map[string]any{"material_id": material, "number": number, "manufacture_date": "2026-11-01"}), "")
	return b["id"].(string)
}

// stockItem is a goods movement's item of qty EA of the material at S1,
// with more.
func (e env) stockItem(material, qty string, more map[string]any) map[string]any {
	it := map[string]any{"material_id": material, "plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": qty, "unit": "EA"}}
	for k, v := range more {
		it[k] = v
	}
	return it
}

// part is a goods issue's entry: "item quantity unit", the batch number
// ("" for none) and the serial numbers. The item is the field as given,
// a number in JSON.
func part(item, batch string, serials ...string) map[string]any {
	f := strings.Fields(item)
	p := map[string]any{"item_no": json.Number(f[0]), "quantity": map[string]any{"value": f[1], "unit": f[2]}}
	if batch != "" {
		p["batch_number"] = batch
	}
	if len(serials) > 0 {
		p["serial_numbers"] = serials
	}
	return p
}

// issueBody is a goods issue's body of the entries.
func issueBody(t *testing.T, parts ...map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"items": parts})
}

// picked is a delivery of the order taking all of it, picked in full:
// its id and ETag.
func (e env) picked(t *testing.T, order string, picks ...string) (string, string) {
	t.Helper()
	d, tag := e.Must(201, "POST", deliveries, e.create(order), "")
	id := d["id"].(string)
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":pick", pick(picks...), tag)
	return id, tag
}

// issueLines are a delivery item's goods issue lines as "line quantity
// batch serials" each, "-" for an item without them.
func issueLines(t *testing.T, item map[string]any) string {
	t.Helper()
	lines := rows(t, item, "issue_lines")
	if len(lines) == 0 {
		return "-"
	}
	var out []string
	for _, l := range lines {
		serials, _ := json.Marshal(l["serial_numbers"])
		out = append(out, fmt.Sprintf("%v %v %v %s", l["line_no"], l["quantity"].(map[string]any)["value"], l["batch_number"], serials))
	}
	return strings.Join(out, "; ")
}

// stockByBatch is the material's stock by batch at S1 as "batch quantity",
// sorted.
func (e env) stockByBatch(t *testing.T, material string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/batch-stock?material_id="+material, "", "")
	var out []string
	for _, r := range rows(t, page, "items") {
		out = append(out, fmt.Sprintf("%v %v", r["batch_number"], r["quantity"].(map[string]any)["value"]))
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// inStock are the material's serial numbers in stock, sorted.
func (e env) inStock(t *testing.T, material string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/serial-numbers?material_id="+material, "", "")
	var out []string
	for _, r := range rows(t, page, "items") {
		if r["in_stock"] == true {
			out = append(out, fmt.Sprint(r["number"]))
		}
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// TestGoodsIssueTracked: a delivery of the sealant, the gauge, the valve
// and the bracket issues the sealant from two batches, the gauge's and
// the valve's units by serial number and the bracket unnamed: the 601's
// lines and the delivery's items name them, the batches and serial
// numbers leave the stock, each item costs what its lines took, and the
// event names them. The reversal (602) gives the batches back what they
// issued and takes the serial numbers into stock again.
func TestGoodsIssueTracked(t *testing.T) {
	e := setupTracked(t)
	order := e.order(t, false, e.ofMaterial(e.sealant, "12 EA"), e.ofMaterial(e.gauge, "2 EA"), e.ofMaterial(e.valve, "1 EA"),
		e.ofMaterial(e.bracket, "1 BOX"))
	id, tag := e.picked(t, order, "1 12 EA", "2 2 EA", "3 1 EA", "4 1 BOX")
	got, gotTag := e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("2 2 EA", "", "G2", "G1"), part("1 10 EA", "L1"),
		part("3 1 EA", "V1", "V-2"), part("1 2 EA", "L2")), tag)
	its := rows(t, got, "items")
	for i, want := range []string{`1 10 L1 null; 2 2 L2 null`, `3 2 <nil> ["G1","G2"]`, `4 1 V1 ["V-2"]`, "-"} {
		if l := issueLines(t, its[i]); l != want {
			t.Errorf("item %d's issue lines: got %s, want %s", i+1, l, want)
		}
	}
	for i, want := range []string{"24.00 EUR", "4.00 EUR", "2.00 EUR", "40.00 EUR"} {
		if c := money(its[i]["cost"]); c != want {
			t.Errorf("item %d's cost: got %s, want %s", i+1, c, want)
		}
	}
	doc, _ := e.Must(200, "GET", "/v1/material-documents/"+got["material_document_id"].(string), "", "")
	var lines []string
	for _, l := range rows(t, doc, "lines") {
		lines = append(lines, fmt.Sprintf("%v %v", l["batch_number"], l["quantity"].(map[string]any)["value"]))
	}
	if s := strings.Join(lines, ", "); s != "L1 -10, L2 -2, <nil> -2, V1 -1, <nil> -20" {
		t.Errorf("the 601's lines: got %s, want the sealant's two batches, the gauge, the valve's batch and the bracket", s)
	}
	for _, w := range []struct{ name, got, want string }{
		{"the sealant's batches", e.stockByBatch(t, e.sealant), "L2 3"}, // the list leaves out a batch holding none
		{"the valve's batch", e.stockByBatch(t, e.valve), "V1 1"},
		{"the gauges in stock", e.inStock(t, e.gauge), "G3"},
		{"the valves in stock", e.inStock(t, e.valve), "V-1"},
	} {
		if w.got != w.want {
			t.Errorf("%s after the goods issue: got %q, want %q", w.name, w.got, w.want)
		}
	}
	ev := e.events(t, id)
	if l, _ := json.Marshal(ev[len(ev)-1]["items"].([]any)[0].(map[string]any)["issue_lines"]); !strings.Contains(string(l), `"batch_number":"L2"`) {
		t.Errorf("the event's first item's issue lines: got %s, want both batches", l)
	}

	rev, _ := e.reverse(t, id, gotTag)
	if l := issueLines(t, rows(t, rev, "items")[0]); l != `1 10 L1 null; 2 2 L2 null` {
		t.Errorf("the reversed delivery's issue lines: got %s, want them kept", l)
	}
	for _, w := range []struct{ name, got, want string }{
		{"the sealant's batches", e.stockByBatch(t, e.sealant), "L1 10, L2 5"},
		{"the valve's batch", e.stockByBatch(t, e.valve), "V1 2"},
		{"the gauges in stock", e.inStock(t, e.gauge), "G1 G2 G3"},
		{"the valves in stock", e.inStock(t, e.valve), "V-1 V-2"},
	} {
		if w.got != w.want {
			t.Errorf("%s after the reversal: got %q, want %q", w.name, w.got, w.want)
		}
	}
}

// TestGoodsIssueTrackedRefusals: the goods issue's entries name an item
// the delivery has, of a batch-managed or serial-managed material, a
// quantity more than zero in its unit, together its picked quantity; a
// picked item of such a material is named; inventory's refusals name the
// entry's own field. Nothing is posted and the delivery stays as it was.
func TestGoodsIssueTrackedRefusals(t *testing.T) {
	e := setupTracked(t)
	order := e.order(t, false, e.ofMaterial(e.sealant, "6 EA"), e.ofMaterial(e.gauge, "2 EA"), e.ofMaterial(e.bracket, "1 BOX"))
	id, tag := e.picked(t, order, "1 6 EA", "2 2 EA", "3 1 BOX")
	gauges := part("2 2 EA", "", "G1", "G2")
	for _, c := range []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"no body", "", 422, "shipping.tracking_required", "items"},
		{"the gauge left out", issueBody(t, part("1 6 EA", "L1")), 422, "shipping.tracking_required", "items"},
		{"an item the delivery lacks", issueBody(t, part("9 1 EA", "L1")), 422, "shipping.unknown_item", "items[0].item_no"},
		{"the bracket named", issueBody(t, gauges, part("3 1 BOX", "")), 422, "shipping.item_not_tracked", "items[1].item_no"},
		{"a quantity of none", issueBody(t, part("1 0 EA", "L1")), 422, "shipping.invalid_quantity", "items[0].quantity.value"},
		{"another unit", issueBody(t, part("1 6 BOX", "L1")), 422, "shipping.unit_mismatch", "items[0].quantity.unit"},
		{"less than picked", issueBody(t, gauges, part("1 4 EA", "L1"), part("1 1 EA", "L2")), 422, "shipping.issue_quantity_mismatch",
			"items[1].quantity.value"},
		{"no such batch", issueBody(t, gauges, part("1 6 EA", "L7")), 422, "inventory.batch_invalid", "items[1].batch_number"},
		{"no batch named", issueBody(t, gauges, part("1 6 EA", "")), 422, "inventory.batch_required", "items[1].batch_number"},
		{"a batch of the gauge", issueBody(t, part("1 6 EA", "L1"), part("2 2 EA", "L1", "G1", "G2")), 422, "inventory.batch_not_allowed",
			"items[1].batch_number"},
		{"too few serial numbers", issueBody(t, part("1 6 EA", "L1"), part("2 2 EA", "", "G1")), 422, "inventory.serial_required",
			"items[1].serial_numbers"},
		{"a serial number twice", issueBody(t, part("1 6 EA", "L1"), part("2 2 EA", "", "G1", "G1")), 422, "inventory.serial_repeated",
			"items[1].serial_numbers[1]"},
		{"part of a unit", issueBody(t, part("1 6 EA", "L1"), part("2 1.5 EA", "", "G1"), part("2 0.5 EA", "", "G2")), 422, "inventory.serial_quantity",
			"items[1].quantity.value"},
		{"a serial number not in stock", issueBody(t, part("1 6 EA", "L1"), part("2 2 EA", "", "G1", "G9")), 409, "inventory.serial_not_in_stock", nil},
		{"more than the batch holds", issueBody(t, gauges, part("1 6 EA", "L2")), 409, "inventory.insufficient_stock", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.Problem(c.status, c.code, "POST", deliveries+"/"+id+":postGoodsIssue", c.body, tag)
			if f := field(m); f != c.field {
				t.Errorf("the field: got %v, want %v", f, c.field)
			}
		})
	}
	m := e.Problem(422, "shipping.tracking_required", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
	if d := fmt.Sprint(m["detail"]); !strings.Contains(d, "managed in batches") {
		t.Errorf("the sealant's refusal: got %q, want it to say it is managed in batches", d)
	}
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, e.gauge)
	if f := field(e.Problem(422, "inventory.material_not_usable", "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 6 EA", "L1"), gauges),
		tag)); f != "items[1].item_no" {
		t.Errorf("a blocked material named: got field %v, want items[1].item_no", f)
	}
	pgtest.Exec(t, `UPDATE materials.material SET status = CASE WHEN id = $1 THEN 'active' ELSE 'blocked' END, version = version + 1
		WHERE id IN ($1, $2)`, e.gauge, e.bracket)
	if f := field(e.Problem(422, "inventory.material_not_usable", "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 6 EA", "L1"), gauges),
		tag)); f != nil {
		t.Errorf("a blocked material the request does not name: got field %v, want none", f)
	}
	after, afterTag := e.Must(200, "GET", deliveries+"/"+id, "", "")
	if after["status"] != "created" || afterTag != tag {
		t.Errorf("the delivery after the refusals: got %v %s, want created %s", after["status"], afterTag, tag)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '601'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("goods issues posted: got %d, want none", n)
	}
}

// TestGoodsIssueTrackingRequired: a picked item's refusal says what its
// material's goods issue names: its batches, its serial numbers, or both.
func TestGoodsIssueTrackingRequired(t *testing.T) {
	e := setupTracked(t)
	for _, c := range []struct{ material, want string }{
		{e.sealant, "names the batches it leaves from"},
		{e.gauge, "names its units' serial numbers"},
		{e.valve, "names its batches and its units' serial numbers"},
	} {
		t.Run(c.want, func(t *testing.T) {
			id, tag := e.picked(t, e.order(t, false, e.ofMaterial(c.material, "1 EA")), "1 1 EA")
			m := e.Problem(422, "shipping.tracking_required", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
			if d := fmt.Sprint(m["detail"]); !strings.Contains(d, c.want) {
				t.Errorf("the detail: got %q, want it to say it %s", d, c.want)
			}
		})
	}
}

// TestGoodsIssueReversalTracked (S2g2): a goods issue whose serial number
// is in stock again since, received by another document, is not
// reversed (409 inventory.serial_in_stock, naming it), and nothing is
// posted; a batch's issue is reversed whatever was taken from the batch
// since.
func TestGoodsIssueReversalTracked(t *testing.T) {
	e := setupTracked(t)
	order := e.order(t, false, e.ofMaterial(e.gauge, "1 EA"))
	id, tag := e.picked(t, order, "1 1 EA")
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":postGoodsIssue", issueBody(t, part("1 1 EA", "", "G1")), tag)
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{
		e.stockItem(e.gauge, "1", map[string]any{"serial_numbers": []string{"G1"}})}}), "")
	m := e.Problem(409, "inventory.serial_in_stock", "POST", deliveries+"/"+id+":reverseGoodsIssue", "", tag)
	if !strings.Contains(fmt.Sprint(m["detail"]), "G1") {
		t.Errorf("the refusal: got %v, want it to name G1", m["detail"])
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '602'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("reversals posted: got %d, want none", n)
	}

	sealed := e.order(t, false, e.ofMaterial(e.sealant, "4 EA"))
	sid, stag := e.picked(t, sealed, "1 4 EA")
	_, stag = e.Must(200, "POST", deliveries+"/"+sid+":postGoodsIssue", issueBody(t, part("1 4 EA", "L2")), stag)
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "201", "items": []any{
		e.stockItem(e.sealant, "1", map[string]any{"batch_id": e.l2, "cost_center_id": e.CostCenterFrom(t, e.DE, "C1", "2026-01-01", nil)})}}), "")
	e.reverse(t, sid, stag)
	if got := e.stockByBatch(t, e.sealant); got != "L1 10, L2 4" {
		t.Errorf("the sealant's batches after the reversal: got %s, want L2's 4 back beside the 1 issued since", got)
	}
}

// TestGoodsIssueTrackedDatabaseFailures: every database error of a goods
// issue naming batches and serial numbers is answered as an error.
func TestGoodsIssueTrackedDatabaseFailures(t *testing.T) {
	e := setupTracked(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.sealant, "2 EA"), e.ofMaterial(e.gauge, "1 EA"))
	id, tag := e.picked(t, order, "1 2 EA", "2 1 EA")
	body := gen.NewOptOutboundDeliveryGoodsIssue(gen.OutboundDeliveryGoodsIssue{Items: []gen.OutboundDeliveryGoodsIssueItem{
		{ItemNo: 1, Quantity: gen.Quantity{Value: "2", Unit: "EA"}, BatchNumber: gen.NewOptString("L1")},
		{ItemNo: 2, Quantity: gen.Quantity{Value: "1", Unit: "EA"}, SerialNumbers: []string{"G1"}}}})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post the goods issue", func(ctx context.Context) error {
		_, err := h.PostOutboundDeliveryGoodsIssue(ctx, body, gen.PostOutboundDeliveryGoodsIssueParams{ID: uuid.MustParse(id), IfMatch: tag})
		return err
	})
}
