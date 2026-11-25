package procurement_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// deliveries posts what TestDeliveryPerformance measures, for the hinge
// at plant P1 of company 2000 from 1 to 31 March 2026, and returns the
// hinge and the plants. The supplier's order of 1 March: item 1 due 6
// March, received on the 5th (on time, 4 days); item 2 due the 7th,
// received the 10th (late, 9 days); item 3 with no delivery date,
// received the 7th (6 days, undated). The vendor's order of 2 March, due
// the 4th, received on the 4th (on time, 2 days). Not counted: a receipt
// of item 1 at plant P2, one of item 2 reversed, a return of item 1, a
// receipt of item 3 on 2 April (outside the window) and the supplier's
// receipt of another material at P1.
func (e env) deliveries(t *testing.T) (hinge, p1, p2 string) {
	t.Helper()
	hinge, other := e.material(t, "DP-HINGE", "active"), e.material(t, "DP-OTHER", "active")
	p1, p2 = e.orgUnit(t, "plant", "P1"), e.orgUnit(t, "plant", "P2")
	order := func(supplier uuid.UUID, day string, its ...string) string {
		o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": %q,
			"items": %s}`, e.DE, supplier, day, items(its...)), "")
		e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
		return id(o)
	}
	receive := func(plant, day, order string, no int, extra string) map[string]any {
		gr, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "plant_id": %q, "document_date": %q, "posting_date": %q%s,
			"items": [{"purchase_order_id": %q, "po_item_no": %d, "quantity": {"value": "2", "unit": "EA"}}]}`, e.DE, plant, day, day, extra, order, no), "")
		return gr
	}
	due := func(day string) string { return fmt.Sprintf(`, "description": "Hinge", "delivery_date": %q`, day) }
	s := order(e.Supplier, "2026-03-01", e.ofMaterial(hinge, "10 EA", due("2026-03-06")), e.ofMaterial(hinge, "10 EA", due("2026-03-07")),
		e.ofMaterial(hinge, "10 EA", `, "description": "Hinge"`), e.ofMaterial(other, "10 EA", due("2026-03-02")))
	v := order(e.Vendor, "2026-03-02", e.ofMaterial(hinge, "10 EA", due("2026-03-04")))
	receive(p1, "2026-03-05", s, 1, "")
	receive(p1, "2026-03-10", s, 2, "")
	receive(p1, "2026-03-07", s, 3, "")
	receive(p1, "2026-03-04", v, 1, "")
	receive(p2, "2026-03-05", s, 1, "")
	reversed := receive(p1, "2026-03-03", s, 2, "")
	e.Must(200, "POST", "/v1/goods-receipts/"+id(reversed)+":reverse", "", "*")
	receive(p1, "2026-03-12", s, 1, `, "movement_type": "122", "reason": "Damaged"`)
	receive(p1, "2026-04-02", s, 3, "")
	receive(p1, "2026-03-05", s, 4, "")
	return hinge, p1, p2
}

// TestDeliveryPerformance (S2l1; ADR-0082 As built S2l1): the suppliers'
// receipts of a material at a plant over the window, measured from posted
// documents: as counted by hand in deliveries, by supplier number, with
// the totals; one supplier alone; another plant's and a window without
// receipts; the default window, the year ending on the company's date.
func TestDeliveryPerformance(t *testing.T) {
	e := setupIn2026(t)
	hinge, p1, p2 := e.deliveries(t)
	read := func(q string) map[string]any {
		got, _ := e.Must(200, "GET", "/v1/supplier-delivery-performance?material_id="+hinge+"&"+q, "", "")
		return got
	}
	march := "plant_id=" + p1 + "&from=2026-03-01&to=2026-03-31"
	for name, c := range map[string]struct{ query, want, totals string }{
		"both suppliers": {march,
			"BP90000001 BP90000001 3 2 1 50 6 6 2026-03-10 measured | BP90000002 BP90000002 1 1 1 100 2 2 2026-03-04 thin", "4 3 2 67"},
		"one supplier":       {march + "&supplier_id=" + e.Vendor.String(), "BP90000002 BP90000002 1 1 1 100 2 2 2026-03-04 thin", "1 1 1 100"},
		"another plant":      {"plant_id=" + p2 + "&from=2026-03-01&to=2026-03-31", "BP90000001 BP90000001 1 1 1 100 5 4 2026-03-05 thin", "1 1 1 100"},
		"no receipts":        {"plant_id=" + p1 + "&from=2026-05-01&to=2026-05-31", "", "0 0 0 -"},
		// The year to 15 June holds the receipt of 2 April too: 32 days, undated.
		"the default window": {"plant_id=" + p1, "BP90000001 BP90000001 4 2 1 50 6 13 2026-04-02 measured | BP90000002 BP90000002 1 1 1 100 2 2 2026-03-04 thin", "5 3 2 67"},
	} {
		t.Run(name, func(t *testing.T) {
			got := read(c.query)
			if rows := deliveryRows(got); rows != c.want {
				t.Errorf("suppliers: got %q, want %q", rows, c.want)
			}
			tot := got["totals"].(map[string]any)
			if totals := fmt.Sprint(tot["receipts"], " ", tot["dated_receipts"], " ", tot["on_time_receipts"], " ", orDash(tot["on_time_percent"])); totals != c.totals {
				t.Errorf("totals: got %q, want %q", totals, c.totals)
			}
		})
	}
	got := read("plant_id=" + p1)
	if window := fmt.Sprint(got["from"], " ", got["to"], " ", got["min_receipts"], " ", got["plant_code"], " ", got["legal_entity_id"]); window !=
		"2025-06-16 2026-06-15 3 P1 "+e.DE.String() {
		t.Errorf("the default window: got %q, want the 365 days to the API's 15 June 2026, min 3, at P1 of company 2000", window)
	}
	first := got["suppliers"].([]any)[1].(map[string]any)
	if _, planned := first["planned_lead_days"]; !planned {
		t.Errorf("the vendor's planned lead time: got none, want 2")
	}
	undated := read("plant_id=" + p1 + "&from=2026-03-07&to=2026-03-07")["suppliers"].([]any)[0].(map[string]any)
	for _, f := range []string{"on_time_percent", "planned_lead_days"} {
		if v, ok := undated[f]; ok {
			t.Errorf("a receipt with no delivery date: got %s %v, want it absent", f, v)
		}
	}
}

// deliveryRows is a performance's suppliers as "number name receipts
// dated on_time percent planned observed last history", joined by " | ".
func deliveryRows(got map[string]any) string {
	var rows []string
	for _, s := range got["suppliers"].([]any) {
		r := s.(map[string]any)
		rows = append(rows, fmt.Sprint(r["supplier_number"], " ", r["supplier_name"], " ", r["receipts"], " ", r["dated_receipts"], " ", r["on_time_receipts"], " ",
			orDash(r["on_time_percent"]), " ", orDash(r["planned_lead_days"]), " ", r["observed_lead_days"], " ", r["last_receipt_date"], " ", r["history"]))
	}
	return strings.Join(rows, " | ")
}

func orDash(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

// TestDeliveryPerformanceRefused: a window that ends before it starts or
// is longer than 1,096 days is 400; an unknown plant, an org unit that
// is no plant, and a plant of a company the caller cannot act in are 422.
func TestDeliveryPerformanceRefused(t *testing.T) {
	e := setupIn2026(t)
	hinge := e.material(t, "DP-HINGE", "active")
	p1, store := e.orgUnit(t, "plant", "P1"), e.orgUnit(t, "storage_location", "S1")
	path := func(plant, window string) string {
		return "/v1/supplier-delivery-performance?material_id=" + hinge + "&plant_id=" + plant + window
	}
	e.Must(200, "GET", path(p1, "&from=2024-01-01&to=2026-12-31"), "", "") // 1,096 days
	for name, c := range map[string]struct {
		status      int
		code, plant string
		window      string
	}{
		"ends before it starts": {400, "request.invalid_parameter", p1, "&from=2026-03-02&to=2026-03-01"},
		"longer than 1096 days": {400, "request.invalid_parameter", p1, "&from=2023-12-31&to=2026-12-31"},
		"unknown plant":         {422, "request.reference_not_found", uuid.NewString(), ""},
		"not a plant":           {422, "request.reference_not_found", store, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "GET", path(c.plant, c.window), "", "")
		})
	}
	t.Run("a company the caller cannot act in", func(t *testing.T) {
		c := e.As(e.Tenant.Agent) // the tenant's agent acts in no company
		c.T = t
		c.Problem(422, "request.reference_not_found", "GET", path(p1, ""), "", "")
	})
}

// TestDeliveryPerformanceFailures: every statement of the read failing
// fails it, and outside a unit of work it fails.
func TestDeliveryPerformanceFailures(t *testing.T) {
	e := setupIn2026(t)
	hinge, p1, _ := e.deliveries(t)
	h := endpoints()
	params := gen.GetSupplierDeliveryPerformanceParams{PlantID: uuid.MustParse(p1), MaterialID: uuid.MustParse(hinge)}
	call := func(ctx context.Context) error {
		_, err := h.GetSupplierDeliveryPerformance(ctx, params)
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "supplier delivery performance", call)
	t.Run("a window given", func(t *testing.T) {
		params.From, params.To = gen.NewOptDate(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), gen.NewOptDate(time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "supplier delivery performance in March", call)
	})
}
