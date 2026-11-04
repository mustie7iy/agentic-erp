package procurement_test

import (
	"fmt"
	"testing"
)

// deliveryDates are the delivery dates of an order's (or change's) items,
// "-" for none.
func deliveryDates(o map[string]any) []string {
	var out []string
	for _, it := range o["items"].([]any) {
		d, ok := it.(map[string]any)["delivery_date"]
		if !ok {
			d = "-"
		}
		out = append(out, fmt.Sprint(d))
	}
	return out
}

// wantDates checks an order's (or change's) items' delivery dates.
func wantDates(t *testing.T, what string, o map[string]any, want string) {
	t.Helper()
	if got := fmt.Sprint(deliveryDates(o)); got != want {
		t.Errorf("%s: got %s, want %s", what, got, want)
	}
}

// TestOrderDeliveryDates: an order item's delivery date (T53g, ADR-0051
// §5) is the one it gives, else its requisition's need date, on creation
// and on a draft's change alike; a header change keeps the items' dates;
// an order change's item that gives none keeps its own, one it adds takes
// the need date; an order of no requisition has none it does not give.
func TestOrderDeliveryDates(t *testing.T) {
	e := setup(t)
	r := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), `, "needed_by": "2026-04-01"`)
	e.act(t, "/v1/requisitions/"+id(r), "submit", "")
	e.Approve(t, id(r))
	o := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", ""), item(e.office, "4", "EA", "2.50", "EUR", `, "delivery_date": "2026-05-05"`)),
		fmt.Sprintf(`, "requisition_id": %q`, id(r)))
	wantDates(t, "created", o, "[2026-04-01 2026-05-05]")
	path := "/v1/purchase-orders/" + id(o)
	_, tag := e.Must(200, "GET", path, "", "")
	o, tag = e.Must(200, "PATCH", path, `{"items": `+items(item(e.office, "10", "EA", "2.50", "EUR", ""),
		item(e.office, "4", "EA", "2.50", "EUR", `, "delivery_date": "2026-05-06"`))+`}`, tag)
	wantDates(t, "a draft's items changed", o, "[2026-04-01 2026-05-06]")
	o, _ = e.Must(200, "PATCH", path, `{"order_date": "2026-03-11"}`, tag)
	wantDates(t, "a draft's header changed", o, "[2026-04-01 2026-05-06]")
	e.act(t, path, "release", "")
	ch := e.submitted(t, path, items(item(e.office, "10", "EA", "2.50", "EUR", `, "delivery_date": "2026-04-15"`), item(e.office, "1", "EA", "2.50", "EUR", ""),
		item(e.office, "1", "EA", "1.00", "EUR", "")))
	wantDates(t, "the change's items", ch, "[2026-04-15 2026-05-06 2026-04-01]")
	if ch["status"] != "applied" {
		t.Errorf("the change: got %v, want applied", ch["status"])
	}
	o, _ = e.Must(200, "GET", path, "", "")
	wantDates(t, "the released order's items", o, "[2026-04-15 2026-05-06 2026-04-01]")
	plain := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	wantDates(t, "an order of no requisition", plain, "[-]")
}
