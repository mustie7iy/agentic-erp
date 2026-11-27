package shipping_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// Checking with the replenishment lead time (S2j2; ADR-0082 As built
// S2j2). The checks run on an injected clock, at noon UTC of 1 January
// 2099 (company 2000's date too), so a lead time of 10 days ends on 11
// January; orders are created through the API, dated the server's day,
// and asked for days in 2099, which are later. P1 holds 200 EA of the
// bracket, 10 of its boxes of 20.

// checkedOn is the day the lead time tests check on.
var checkedOn = time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)

// clocked are the sales context's endpoints on the checks' clock.
func clocked(t *testing.T) *sales.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Sales(app.Config{Pages: pages, Now: func() time.Time { return checkedOn }})
}

// committed runs fn in a request as the tenant's human, committed.
func (e env) committed(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
		t.Fatal(err)
	}
}

// leadTime plans the material at P1 with a replenishment lead time of
// days and no planning.
func (e env) leadTime(t *testing.T, material string, days int32) {
	t.Helper()
	e.committed(t, func(ctx context.Context) error {
		_, err := app.MRP(app.Config{}).CreatePlan(ctx, mrp.PlanInput{PlantID: e.p1, MaterialID: uuid.MustParse(material), AccountID: e.revenue,
			MRPType: mrp.NoPlanning, ReorderPoint: "0", LeadDays: &days})
		return err
	})
}

// askedFor is placedAt P1 asked for delivery on the date ("": none).
func (e env) askedFor(t *testing.T, date string, items ...string) uuid.UUID {
	t.Helper()
	requested := ""
	if date != "" {
		requested = fmt.Sprintf(`"requested_delivery_date": %q, `, date)
	}
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q, %s
		"items": [%s]}`, e.DE, e.customer, e.p1, requested, strings.Join(items, ", ")), "")
	return uuid.MustParse(o["id"].(string))
}

// confirmedOn confirms the draft on the checks' clock and reads it.
func (e env) confirmedOn(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	e.committed(t, func(ctx context.Context) error {
		_, err := clocked(t).ConfirmOrder(ctx, id, etag.IfMatch(`"1"`))
		return err
	})
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+id.String(), "", "")
	return o
}

// changedOn changes the confirmed order on the checks' clock and reads it.
func (e env) changedOn(t *testing.T, id uuid.UUID, in sales.OrderChangeInput) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/sales-orders/"+id.String(), "", "")
	e.committed(t, func(ctx context.Context) error {
		_, err := clocked(t).ChangeOrder(ctx, id, etag.IfMatch(tag), in)
		return err
	})
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+id.String(), "", "")
	return o
}

// onLeadOf is what rests on the lead time of the order's items, "-" for
// an item with nothing on it, by item.
func onLeadOf(t *testing.T, o map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, o, "items") {
		q, ok := it["lead_time_quantity"].(map[string]any)
		if !ok {
			out = append(out, fmt.Sprintf("- %v", it["lead_time_date"]))
			continue
		}
		out = append(out, fmt.Sprintf("%v %v until %v", q["value"], q["unit"], it["lead_time_date"]))
	}
	return strings.Join(out, ", ")
}

// TestConfirmationOnTheLeadTime: an order asked for on or after the
// company's date today plus the material's lead time at the plant is
// confirmed in full, the stock first and the rest on the lead time until
// it ends, which counts as promised; one asked for a day sooner, or for
// no date, and a material without a lead time, get the stock alone. The
// audit record says what lead time each item was judged on.
func TestConfirmationOnTheLeadTime(t *testing.T) {
	e := setup(t)
	e.leadTime(t, e.bracket, 10)
	unplanned := e.material(t, "Unplanned bracket", "trading_good")
	for _, c := range []struct {
		name, date, item, want, wantOnLead string
	}{
		{"asked for the day the lead time ends", "2099-01-11", e.ofMaterial(e.bracket, "12 BOX"), "12 BOX", "2 BOX until 2099-01-11"},
		{"asked for a day sooner", "2099-01-10", e.ofMaterial(e.bracket, "1 BOX"), "0 BOX", "- <nil>"},
		{"asked for no date", "", e.ofMaterial(e.bracket, "1 BOX"), "0 BOX", "- <nil>"},
		{"asked for later, nothing left in stock", "2099-03-01", e.ofMaterial(e.bracket, "3 EA"), "3 EA", "3 EA until 2099-01-11"},
		{"a material without a lead time", "2099-03-01", e.ofMaterial(unplanned, "1 EA"), "0 EA", "- <nil>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := e.confirmedOn(t, e.askedFor(t, c.date, c.item))
			if got := confirmedOf(t, o); got != c.want {
				t.Errorf("confirmed: got %s, want %s", got, c.want)
			}
			if got := onLeadOf(t, o); got != c.wantOnLead {
				t.Errorf("on the lead time: got %s, want %s", got, c.wantOnLead)
			}
		})
	}
	t.Run("audited", func(t *testing.T) {
		id := e.askedFor(t, "2099-02-01", e.ofMaterial(e.bracket, "1 EA"))
		e.confirmedOn(t, id)
		a := e.audits(t, id.String(), "sales.sales_order.confirm")
		if len(a) != 1 {
			t.Fatalf("got %d audit records, want 1", len(a))
		}
		item := a[0]["availability"].(map[string]any)["items"].([]any)[0].(map[string]any)
		if got := fmt.Sprint(item["lead_time_days"], " ", item["lead_time"], " ", item["lead_time_before"]); got !=
			"10 map[date:2099-01-11T00:00:00Z quantity:1] <nil>" {
			t.Errorf("the item's check: got %s, want its 10 days and 1 EA until 11 January", got)
		}
	})
}

// TestLeadTimeOnAChange: a change of a confirmed order checks the items
// it changes for the date it asks for: one asked for sooner than the lead
// time keeps what it had confirmed with what of it rested on the lead
// time, less the stock that arrived since; one asked for in time again is
// confirmed in full, the rest on the lead time from the change's day.
func TestLeadTimeOnAChange(t *testing.T) {
	e := setup(t)
	e.leadTime(t, e.bracket, 10)
	id := e.askedFor(t, "2099-01-20", e.ofMaterial(e.bracket, "12 BOX"))
	if got := onLeadOf(t, e.confirmedOn(t, id)); got != "2 BOX until 2099-01-11" {
		t.Fatalf("confirmed: got %s, want 2 BOX on the lead time", got)
	}
	day := func(s string) *time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return &d
	}
	boxes := func(q string) []sales.ItemChangeInput {
		unit := "BOX"
		return []sales.ItemChangeInput{{ItemNo: 1, Quantity: &q, Unit: &unit}}
	}
	for _, c := range []struct {
		name, stock string
		in          sales.OrderChangeInput
		want        string
	}{
		{"raised and asked for sooner", "", sales.OrderChangeInput{Items: boxes("13"), DeliveryDateSet: true, DeliveryDate: day("2099-01-05")},
			"12 BOX: 2 BOX until 2099-01-11"},
		{"raised again once a box arrived", "20", sales.OrderChangeInput{Items: boxes("14")}, "12 BOX: 1 BOX until 2099-01-11"},
		{"raised and asked for in time", "", sales.OrderChangeInput{Items: boxes("15"), DeliveryDateSet: true, DeliveryDate: day("2099-02-01")},
			"15 BOX: 4 BOX until 2099-01-11"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.stock != "" {
				e.receive(t, c.stock)
			}
			o := e.changedOn(t, id, c.in)
			if got := confirmedOf(t, o) + ": " + onLeadOf(t, o); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestLeadTimeDatabaseFailures: a confirmation that reads lead times
// reports each of its statements' failures.
func TestLeadTimeDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.leadTime(t, e.bracket, 10)
	id := e.askedFor(t, "2099-02-01", e.ofMaterial(e.bracket, "1 BOX"))
	h := clocked(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "confirm on the lead time", func(ctx context.Context) error {
		_, err := h.ConfirmOrder(ctx, id, etag.IfMatch(`"1"`))
		return err
	})
}
