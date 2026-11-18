package inventory_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestPostMovements: each movement type a person posts moves the stock it
// names, numbered from the company's range, dated the company's date, its
// lines in the base unit and signed, its items as entered.
func TestPostMovements(t *testing.T) {
	e := setup(t)
	center := map[string]any{"cost_center_id": e.c1}
	steps := []struct {
		name, movementType string
		item               map[string]any
		lines              string
		s1, s2, s3         string
	}{
		{"an initial entry of 5 boxes", "561", e.item("5", "BOX", nil), "P1:S1 100 EA (5 BOX)", "100 EA", "", ""},
		{"a receipt of 2.5 each", "501", e.item("2.5", "EA", nil), "P1:S1 2.5 EA (2.5 EA)", "102.5 EA", "", ""},
		{"an issue of 10 to a cost center", "201", e.item("10", "EA", center), "P1:S1 -10 EA (10 EA)", "92.5 EA", "", ""},
		{"a scrapping of 3 kilograms", "551", e.item("3", "KG", center), "P1:S1 -1 EA (3 KG)", "91.5 EA", "", ""},
		{"a transfer of 40 to S2", "311", e.item("40", "EA", map[string]any{"to_storage_location_id": e.s2}), "P1:S1 -40 EA (40 EA); P1:S2 40 EA (40 EA)",
			"51.5 EA", "40 EA", ""},
		{"a transfer of a box to P2", "301", e.item("1", "BOX", map[string]any{"plant_id": e.p1, "storage_location_id": e.s2, "to_plant_id": e.p2,
			"to_storage_location_id": e.s3}), "P1:S2 -20 EA (1 BOX); P2:S3 20 EA (1 BOX)", "51.5 EA", "20 EA", "20 EA"},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			d := e.post(t, s.movementType, s.item)
			if got := e.lines(t, d); got != s.lines {
				t.Errorf("lines: got %s, want %s", got, s.lines)
			}
			for _, w := range []struct{ name, got, want string }{
				{"movement type", fmt.Sprint(d["movement_type"]), s.movementType},
				{"company", fmt.Sprint(d["company_code"]), "2000"},
				{"S1", e.stock(t, e.s1, ""), s.s1}, {"S2", e.stock(t, e.s2, ""), s.s2}, {"S3", e.stock(t, e.s3, ""), s.s3},
			} {
				if w.got != w.want {
					t.Errorf("%s: got %q, want %q", w.name, w.got, w.want)
				}
			}
		})
	}
}

// TestPostRecorded: a posting answers its document with its number,
// names and ETag, records its audit without the header text and
// publishes MaterialDocumentPosted naming what its lines move.
func TestPostRecorded(t *testing.T) {
	e := setup(t)
	got, tag := e.Must(201, "POST", docs, mustJSON(t, map[string]any{"movement_type": "561", "header_text": "Count of Ada Lovelace's shelf",
		"items": []any{e.item("5", "EA", nil)}}), "")
	id := got["id"].(string)
	read, readTag := e.Must(200, "GET", docs+"/"+id, "", "")
	for _, w := range []struct{ name, got, want string }{
		{"number", fmt.Sprint(got["number"])[:2], "MD"},
		{"movement type name", fmt.Sprint(got["movement_type_name"]), "Initial entry of stock"},
		{"header text", fmt.Sprint(read["header_text"]), "Count of Ada Lovelace's shelf"},
		{"ETag", tag, readTag},
		{"material", fmt.Sprint(rows(t, read, "lines")[0]["material_description"]), "Machined bracket M10"},
		{"plant name", fmt.Sprint(rows(t, read, "lines")[0]["plant_name"]), "Unit P1"},
	} {
		if w.got != w.want {
			t.Errorf("%s: got %q, want %q", w.name, w.got, w.want)
		}
	}
	audits := e.audits(t, id, "inventory.material_document.post")
	if len(audits) != 1 || audits[0]["has_header_text"] != true || fmt.Sprint(audits[0]) == "" {
		t.Fatalf("audit: got %v, want one with has_header_text", audits)
	}
	if s := fmt.Sprint(audits[0]); strings.Contains(s, "Lovelace") {
		t.Errorf("audit: got %s, want no header text in it", s)
	}
	events := e.events(t, id)
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	line := rows(t, events[0], "lines")[0]
	if got, want := fmt.Sprintf("%v %v %v:%v %v", events[0]["legal_entity_code"], line["material_number"], line["plant_code"], line["storage_location_code"],
		line["quantity"]), fmt.Sprintf("2000 %v P1:S1 5", rows(t, read, "lines")[0]["material_number"]); got != want {
		t.Errorf("event: got %s, want %s", got, want)
	}
}

// TestPostRefused: what a posting is refused for, each naming its field.
func TestPostRefused(t *testing.T) {
	e := setup(t)
	pending := e.material(t, "Pending bracket", "trading_good", false)
	service := e.material(t, "Assembly service", "service", true)
	s5 := e.orgUnit(t, e.DE, "storage_location", "S5", &e.p1)
	e.post(t, "561", e.item("5", "EA", nil))
	center := map[string]any{"cost_center_id": e.c1}
	cases := []struct {
		name, movementType string
		item               map[string]any
		status             int
		code, field        string
	}{
		{"a reversal type", "562", e.item("1", "EA", nil), 422, "inventory.unknown_movement_type", "movement_type"},
		{"an unknown type", "101", e.item("1", "EA", nil), 422, "inventory.unknown_movement_type", "movement_type"},
		{"no material", "561", e.item("1", "EA", map[string]any{"material_id": e.s1}), 422, "inventory.material_invalid", "items[0].material_id"},
		{"a pending material", "561", e.item("1", "EA", map[string]any{"material_id": pending}), 422, "inventory.material_not_usable", "items[0].material_id"},
		{"a service", "561", e.item("1", "EA", map[string]any{"material_id": service}), 422, "inventory.material_not_stocked", "items[0].material_id"},
		{"no plant", "561", e.item("1", "EA", map[string]any{"plant_id": e.s1}), 422, "inventory.plant_invalid", "items[0].plant_id"},
		{"a plant it is not extended to", "561", e.item("1", "EA", map[string]any{"plant_id": e.p3, "storage_location_id": e.s4}), 422,
			"inventory.plant_not_extended", "items[0].plant_id"},
		{"a storage location of another plant", "561", e.item("1", "EA", map[string]any{"storage_location_id": e.s3}), 422,
			"inventory.storage_location_invalid", "items[0].storage_location_id"},
		{"a zero quantity", "561", e.item("0", "EA", nil), 422, "inventory.invalid_quantity", "items[0].quantity.value"},
		{"a unit it has not", "561", e.item("1", "PC", nil), 422, "inventory.unknown_unit", "items[0].quantity.unit"},
		{"an inexact quantity", "561", e.item("1", "KG", nil), 422, "inventory.inexact_quantity", "items[0].quantity.value"},
		{"an issue without a cost center", "201", e.item("1", "EA", nil), 422, "inventory.cost_center_required", "items[0].cost_center_id"},
		{"a receipt with a cost center", "501", e.item("1", "EA", center), 422, "inventory.cost_center_not_allowed", "items[0].cost_center_id"},
		{"another company's cost center", "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c9}), 422, "inventory.cost_center_invalid",
			"items[0].cost_center_id"},
		{"no cost center", "551", e.item("1", "EA", map[string]any{"cost_center_id": e.s1}), 422, "inventory.cost_center_invalid", "items[0].cost_center_id"},
		{"a receiving location of a receipt", "501", e.item("1", "EA", map[string]any{"to_storage_location_id": e.s2}), 422, "inventory.transfer_not_allowed",
			"items[0].to_storage_location_id"},
		{"a receiving plant of a 311", "311", e.item("1", "EA", map[string]any{"to_plant_id": e.p2, "to_storage_location_id": e.s3}), 422,
			"inventory.transfer_not_allowed", "items[0].to_plant_id"},
		{"a 311 without a receiving location", "311", e.item("1", "EA", nil), 422, "inventory.transfer_target_required", "items[0].to_storage_location_id"},
		{"a 301 without a receiving plant", "301", e.item("1", "EA", map[string]any{"to_storage_location_id": e.s3}), 422, "inventory.transfer_target_required",
			"items[0].to_plant_id"},
		{"a 311 to itself", "311", e.item("1", "EA", map[string]any{"to_storage_location_id": e.s1}), 422, "inventory.same_location",
			"items[0].to_storage_location_id"},
		{"a 301 to its own plant", "301", e.item("1", "EA", map[string]any{"to_plant_id": e.p1, "to_storage_location_id": e.s2}), 422,
			"inventory.same_location", "items[0].to_plant_id"},
		{"a 301 to a plant it is not kept at", "301", e.item("1", "EA", map[string]any{"to_plant_id": e.p3, "to_storage_location_id": e.s4}), 422,
			"inventory.plant_not_extended", "items[0].to_plant_id"},
		{"a 311 to a location it is not kept at", "311", e.item("1", "EA", map[string]any{"to_storage_location_id": s5}), 422,
			"inventory.storage_location_not_extended", "items[0].to_storage_location_id"},
		{"a location it is not kept at", "561", e.item("1", "EA", map[string]any{"storage_location_id": s5}), 422,
			"inventory.storage_location_not_extended", "items[0].storage_location_id"},
		{"an issue of more than the stock", "201", e.item("5.000001", "EA", center), 409, "inventory.insufficient_stock", ""},
		{"a stock of 15 digits", "561", e.item("99999999999999", "BOX", nil), 422, "inventory.inexact_quantity", "items[0].quantity.value"},
		{"a stock beyond a quantity column", "561", e.item("99999999999996", "EA", nil), 422, "inventory.quantity_too_large", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", docs, body(t, c.movementType, c.item), "")
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
			if f := field(got); c.field != "" && f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
		})
	}
	if got := e.stock(t, e.s1, ""); got != "5 EA" {
		t.Errorf("stock after the refusals: got %q, want 5 EA", got)
	}
}

// TestPostMixedCompanies: a document moves one company's stock, a
// transfer between plants stays in one company, and a cost center out of
// force on the posting date is refused.
func TestPostMixedCompanies(t *testing.T) {
	e := setup(t)
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Bracket of two companies",
		"base_unit": "EA", "material_type": "trading_good", "material_group": "BRACKETS", "plants": []any{
			map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}, map[string]any{"plant_id": e.p3, "storage_location_ids": []any{e.s4}}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	both := map[string]any{"material_id": m["id"]}
	at3 := map[string]any{"material_id": m["id"], "plant_id": e.p3, "storage_location_id": e.s4}
	for _, c := range []struct {
		name, movementType string
		items              []map[string]any
		field              string
	}{
		{"items of two companies", "561", []map[string]any{e.item("1", "EA", both), e.item("1", "EA", at3)}, "items[1].plant_id"},
		{"a transfer to another company's plant", "301", []map[string]any{e.item("1", "EA", map[string]any{"material_id": m["id"], "to_plant_id": e.p3,
			"to_storage_location_id": e.s4})}, "items[0].to_plant_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", docs, body(t, c.movementType, c.items...), "")
			if status != 422 || got["code"] != "inventory.mixed_companies" || field(got) != c.field {
				t.Errorf("got %d %v, want 422 inventory.mixed_companies naming %s", status, got, c.field)
			}
		})
	}
	later := e.CostCenterFrom(t, e.DE, "C2", "2099-01-01", nil)
	status, got, _ := e.Do("POST", docs, body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": later})), "")
	if status != 422 || got["code"] != "inventory.cost_center_invalid" {
		t.Errorf("a cost center out of force: got %d %v, want 422 inventory.cost_center_invalid", status, got)
	}
}

// TestIssuesAtOnce: two issues of the last quantity at once post one; the
// other is refused, and the stock is never below zero.
func TestIssuesAtOnce(t *testing.T) {
	e := setup(t)
	e.post(t, "561", e.item("3", "EA", nil))
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", docs, body(t, "201", e.item("2", "EA", map[string]any{"cost_center_id": e.c1})), "")
		}()
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
		t.Errorf("statuses: got %v, want one 201 and one 409", statuses)
	}
	if got := e.stock(t, e.s1, ""); got != "1 EA" {
		t.Errorf("stock: got %q, want 1 EA", got)
	}
}

// TestTransfersInOppositeOrder: transfers between S1 and S2 either way
// round at once all post, none waiting on another for good.
func TestTransfersInOppositeOrder(t *testing.T) {
	e := setup(t)
	e.post(t, "561", e.item("10", "EA", nil), e.item("10", "EA", map[string]any{"storage_location_id": e.s2}))
	var wg sync.WaitGroup
	statuses := make([]int, 20)
	for i := range statuses {
		from, to := e.s1, e.s2
		if i%2 == 1 {
			from, to = e.s2, e.s1
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", docs, body(t, "311", e.item("1", "EA", map[string]any{"storage_location_id": from, "to_storage_location_id": to})), "")
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 201 {
			t.Errorf("transfer %d: got %d, want 201", i, s)
		}
	}
	if got := e.stock(t, e.s1, "") + " " + e.stock(t, e.s2, ""); got != "10 EA 10 EA" {
		t.Errorf("stock: got %s, want 10 EA at each", got)
	}
}

// TestCircularTransfers: a document whose items would take a location
// below zero line by line is refused, though its net change is none;
// the reversal of a chain of transfers posts, its lines last first.
func TestCircularTransfers(t *testing.T) {
	e := setup(t)
	e.post(t, "561", e.item("10", "EA", map[string]any{"storage_location_id": e.s2}))
	status, got, _ := e.Do("POST", docs, body(t, "311", e.item("10", "EA", map[string]any{"to_storage_location_id": e.s2}),
		e.item("10", "EA", map[string]any{"storage_location_id": e.s2, "to_storage_location_id": e.s1})), "")
	if status != 409 || got["code"] != "inventory.insufficient_stock" {
		t.Errorf("a circle from an empty location: got %d %v, want 409 inventory.insufficient_stock", status, got)
	}
	chain := e.post(t, "311", e.item("10", "EA", map[string]any{"storage_location_id": e.s2, "to_storage_location_id": e.s1}),
		e.item("10", "EA", map[string]any{"to_storage_location_id": e.s2}))
	_, tag := e.Must(200, "GET", docs+"/"+chain["id"].(string), "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+chain["id"].(string)+":reverse", "", tag)
	if got := e.lines(t, rev); got != "P1:S2 -10 EA (10 EA); P1:S1 10 EA (10 EA); P1:S1 -10 EA (10 EA); P1:S2 10 EA (10 EA)" {
		t.Errorf("the reversal's lines: got %s, want the original's last first", got)
	}
	if got := e.stock(t, e.s1, "") + "|" + e.stock(t, e.s2, ""); got != "|10 EA" {
		t.Errorf("after the chain's reversal: got %s, want |10 EA", got)
	}
}
