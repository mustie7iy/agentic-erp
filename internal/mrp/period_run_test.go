package mrp_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// confirmedOn creates and confirms Northwind's order of the bracket from
// P1 of quantity EA, asked for on the day ("" for none, due on its
// date), and returns the order.
func (e env) confirmedOn(t *testing.T, quantity, day string) map[string]any {
	t.Helper()
	requested := ""
	if day != "" {
		requested = fmt.Sprintf(`, "requested_delivery_date": %q`, day)
	}
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q%s,
		"items": [{"material_id": %q, "quantity": {"value": %q, "unit": "EA"}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`,
		e.DE, e.customer, e.p1, requested, e.bracket, quantity, e.revenue), "")
	e.Must(200, "POST", "/v1/sales-orders/"+o["id"].(string)+":confirm", "", `"1"`)
	return o
}

// schedule is a line's lots as "quantity@needed_by ...", "-" for none.
func schedule(t *testing.T, r map[string]any, material string) string {
	t.Helper()
	for _, l := range rows(t, r, "lines") {
		if l["material_id"] == material {
			var out []string
			for _, x := range rows(t, l, "lot_schedule") {
				out = append(out, fmt.Sprint(x["quantity"].(map[string]any)["value"], "@", x["needed_by"]))
			}
			if len(out) == 0 {
				return "-"
			}
			return strings.Join(out, " ")
		}
	}
	return "none"
}

// TestPlanningRunPeriodLots (ADR-0084 As built, period lot sizes): the
// bracket, reorder point 50 at P1 in weekly lots rounded to 10, has 200
// EA in stock against orders of 150 EA due today, 30 EA a week later and
// 25 EA two weeks later. Today's 150 leave it at 50; the next week's 30
// take it to 20, 30 below, one lot of 30 needed on that order's day; the
// week after, 25 take it to 25, a lot of 25 rounded to 30. The run
// proposes them as the two items of one requisition needed by the first
// lot's day, the line keeping each lot's day; its shortage is the
// reorder point procedure's, 55. Sized lot for lot again, a rerun
// proposes the shortage in one lot needed today, with no schedule.
func TestPlanningRunPeriodLots(t *testing.T) {
	e := setup(t)
	first := e.confirmedOn(t, "150", "")
	today, err := time.Parse(time.DateOnly, first["order_date"].(string))
	if err != nil {
		t.Fatal(err)
	}
	week, fortnight := today.AddDate(0, 0, 7).Format(time.DateOnly), today.AddDate(0, 0, 14).Format(time.DateOnly)
	e.confirmedOn(t, "30", week)
	e.confirmedOn(t, "25", fortnight)
	plan, tag := e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket, "mrp_type": "reorder_point",
		"reorder_point": "50", "account_id": e.stock, "lot_size": "weekly", "rounding_value": "10"}), "")
	if got := fmt.Sprint(plan["lot_size"], " ", plan["rounding_value"]); got != "weekly map[unit:EA value:10]" {
		t.Errorf("the plan: got %s, want weekly lots rounded to 10 EA", got)
	}
	r := e.run(t)
	if got := line(t, r, e.bracket); got != "205 200 0 55 PR100000" {
		t.Errorf("the line: got %s, want a demand of 205 against 200 in stock, 55 short", got)
	}
	if got := lotLine(t, r, e.bracket); got != "weekly 60 2 PR100000" {
		t.Errorf("the line's lots: got %s, want 60 in two weekly lots", got)
	}
	if got, want := schedule(t, r, e.bracket), "30@"+week+" 30@"+fortnight; got != want {
		t.Errorf("the schedule: got %s, want %s", got, want)
	}
	if got := e.items(t, r, e.bracket); got != "30 EA, 30 EA" {
		t.Errorf("the requisition's items: got %s, want the two lots", got)
	}
	if got, _ := e.requisition(t, rows(t, r, "lines")[0]["requisition_id"].(string)); !strings.Contains(got, " "+week+" ") {
		t.Errorf("the requisition: got %s, want it needed by the first lot's day %s", got, week)
	}
	e.Must(200, "PATCH", plans+"/"+plan["id"].(string), `{"lot_size": "lot_for_lot", "rounding_value": null}`, tag)
	again := e.run(t)
	if got := lotLine(t, again, e.bracket) + " " + schedule(t, again, e.bracket); got != "lot_for_lot 55 1 PR100001 -" {
		t.Errorf("lot for lot: got %s, want the shortage in one lot, no schedule", got)
	}
	if got, _ := e.requisition(t, rows(t, again, "lines")[0]["requisition_id"].(string)); !strings.Contains(got, " "+today.Format(time.DateOnly)+" ") {
		t.Errorf("lot for lot's requisition: got %s, want it needed by the run's day", got)
	}
	listed, _ := e.Must(200, "GET", runs+"?plant_id="+e.p1.String(), "", "")
	if got, want := schedule(t, rows(t, listed, "items")[0], e.bracket), "30@"+week+" 30@"+fortnight; got != want {
		t.Errorf("the first run listed: got %s, want its schedule %s", got, want)
	}
}

// TestPlanningRunPeriodTooManyLots: a weekly plan whose maximum lot size
// splits a week's need into more than 100 lots fails the run with
// mrp.too_many_lots, proposing nothing.
func TestPlanningRunPeriodTooManyLots(t *testing.T) {
	e := setup(t)
	e.confirmedOn(t, "201.01", "")
	e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket, "mrp_type": "reorder_point",
		"reorder_point": "0", "account_id": e.stock, "lot_size": "weekly", "maximum_lot_size": "0.01"}), "")
	r := e.run(t)
	errObj, _ := r["error"].(map[string]any)
	if got := fmt.Sprint(r["status"], " ", errObj["code"]); got != "failed mrp.too_many_lots" {
		t.Errorf("got %s, want the run failed with mrp.too_many_lots", got)
	}
	if got := e.proposals(t); len(got) != 0 {
		t.Errorf("the proposals: got %v, want none", got)
	}
}
