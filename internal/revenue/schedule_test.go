package revenue_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/revenue"
)

// endpointsAt are the context's endpoints with the clock at noon UTC of
// the day (YYYY-MM-DD): company 2000 sets no time zone, so that is its
// date today.
func endpointsAt(t *testing.T, day string) *revenue.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	return app.Revenue(app.Config{Pages: pages, Now: func() time.Time { return at.Add(12 * time.Hour) }})
}

// delivered confirms the quantity of the order's item as delivered on
// the day, which is in the past: a delivery is not dated after today.
func (e env) delivered(t *testing.T, order string, item int, quantity, day string) {
	t.Helper()
	e.Must(201, "POST", "/v1/sales-orders/"+order+"/deliveries",
		fmt.Sprintf(`{"delivery_date": %q, "items": [{"item_no": %d, "quantity": {"value": %q, "unit": "EA"}}]}`, day, item, quantity), "")
}

// activated activates the contract at its current version.
func (e env) activated(t *testing.T, id string) {
	t.Helper()
	_, tag := e.Must(200, "GET", contracts+"/"+id, "", "")
	e.Must(200, "POST", contracts+"/"+id+":activate", "", tag)
}

// scheduleAt is the contract's schedule read with the company's date
// today being the day, or the read's error.
func (e env) scheduleAt(t *testing.T, id, day string) (*gen.RevenueSchedule, error) {
	t.Helper()
	h := endpointsAt(t, day)
	var out *gen.RevenueSchedule
	err := e.inRequest(t, func(ctx context.Context) error {
		res, err := h.GetRevenueSchedule(ctx, gen.GetRevenueScheduleParams{ID: uuid.MustParse(id)})
		if err == nil {
			out = res.(*gen.RevenueSchedule)
		}
		return err
	})
	return out, err
}

// mustSchedule is scheduleAt without an error.
func (e env) mustSchedule(t *testing.T, id, day string) *gen.RevenueSchedule {
	t.Helper()
	s, err := e.scheduleAt(t, id, day)
	if err != nil {
		t.Fatalf("schedule of %s at %s: got %v, want no error", id, day, err)
	}
	return s
}

// rows writes a schedule's periods as "year/period amount recognized
// planned [days|quantity unit]", joined by "; ".
func rows(periods []gen.RevenueSchedulePeriod) string {
	out := make([]string, len(periods))
	for i, p := range periods {
		out[i] = fmt.Sprintf("%d/%d %s %s %s", p.FiscalYear, p.FiscalPeriod, p.Amount.Amount, p.RecognizedAmount.Amount, p.PlannedAmount.Amount)
		if d, ok := p.Days.Get(); ok {
			out[i] += fmt.Sprintf(" %dd", d)
		}
		if q, ok := p.DeliveredQuantity.Get(); ok {
			out[i] += " " + q.Value + " " + q.Unit
		}
	}
	return strings.Join(out, "; ")
}

// sums writes an obligation's sums as "allocated = recognized + planned
// + unscheduled".
func sums(o gen.RevenueScheduleObligation) string {
	return fmt.Sprintf("%s = %s + %s + %s", o.AllocatedAmount.Amount, o.RecognizedAmount.Amount, o.PlannedAmount.Amount, o.UnscheduledAmount.Amount)
}

// The straight line of 1200.00 over 2026's 365 days: 1200.00 × days ÷ 365
// per month, floored, leaves six cents, which go to the largest
// remainders, the months of 31 days, the earlier first: December, the
// seventh, gets none.
const line2026 = "2026/1 101.92; 2026/2 92.05; 2026/3 101.92; 2026/4 98.63; 2026/5 101.92; 2026/6 98.63; 2026/7 101.92; 2026/8 101.92; " +
	"2026/9 98.63; 2026/10 101.92; 2026/11 98.63; 2026/12 101.91"

// amounts writes the periods as "year/period amount", joined by "; ".
func amounts(periods []gen.RevenueSchedulePeriod) string {
	out := make([]string, len(periods))
	for i, p := range periods {
		out[i] = fmt.Sprintf("%d/%d %s", p.FiscalYear, p.FiscalPeriod, p.Amount.Amount)
	}
	return strings.Join(out, "; ")
}

// TestSchedule: an active contract's schedule has a point-in-time
// obligation's allocated amount in the fiscal periods of its deliveries,
// in proportion to the quantity delivered, the undelivered quantity's
// share in no period and the last delivery taking the remainder; an
// over-time obligation's amount is spread over its service period's days
// per fiscal period, the shares adding up exactly; amounts are split at
// the company's date today; the contract's periods are the obligations'
// added up (T81c acceptance).
func TestSchedule(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support(), e.item("Fitting kit", "KIT", "3", "100.00", "EUR"))
	path := contracts + "/" + id
	e.Must(200, "PATCH", path, set(overTime(2, "2026-01-01", "2026-12-31", "1300.00")), `"1"`)
	e.activated(t, id)
	// 3000.00 over stand-alone selling prices of 1500.00, 1300.00 and 300.00.
	e.delivered(t, order, 1, "50", "2026-01-15")
	e.delivered(t, order, 1, "30", "2026-01-20")
	e.delivered(t, order, 3, "3", "2026-03-05")

	t.Run("part delivered, in February", func(t *testing.T) {
		s := e.mustSchedule(t, id, "2026-02-10")
		if got := s.AsOf.Format(time.DateOnly); got != "2026-02-10" {
			t.Errorf("as_of: got %s, want 2026-02-10", got)
		}
		goods, support, kit := s.Obligations[0], s.Obligations[1], s.Obligations[2]
		// 1451.61 × 50/120 = 604.8375 → 604.84; × 80/120 = 967.74, the second delivery adding 362.90.
		if got, want := rows(goods.Periods), "2026/1 967.74 967.74 0.00 80 EA"; got != want {
			t.Errorf("the goods' periods: got %s, want %s", got, want)
		}
		if got, want := sums(goods), "1451.61 = 967.74 + 0.00 + 483.87"; got != want {
			t.Errorf("the goods' sums: got %s, want %s", got, want)
		}
		if got, want := sums(support), "1258.07 = 141.32 + 1116.75 + 0.00"; got != want {
			t.Errorf("the support's sums: got %s, want %s", got, want)
		}
		// February has 96.51 for 28 days; 10 of them are 34.4678..., whose remainder is larger than the rest's.
		if got, want := rows(support.Periods[:3]), "2026/1 106.85 106.85 0.00 31d; 2026/2 96.51 34.47 62.04 28d; 2026/3 106.85 0.00 106.85 31d"; got != want {
			t.Errorf("the support's first periods: got %s, want %s", got, want)
		}
		// The kit's delivery is dated after the day the schedule is told on: planned.
		if got, want := rows(kit.Periods), "2026/3 290.32 0.00 290.32 3 EA"; got != want {
			t.Errorf("the kit's periods: got %s, want %s", got, want)
		}
		if got, want := fmt.Sprintf("%s = %s + %s + %s", s.TransactionPrice.Amount, s.RecognizedAmount.Amount, s.PlannedAmount.Amount, s.UnscheduledAmount.Amount),
			"3000.00 = 1109.06 + 1407.07 + 483.87"; got != want {
			t.Errorf("the contract's sums: got %s, want %s", got, want)
		}
		if got, want := rows(s.Periods[:3]), "2026/1 1074.59 1074.59 0.00; 2026/2 96.51 34.47 62.04; 2026/3 397.17 0.00 397.17"; got != want {
			t.Errorf("the contract's first periods: got %s, want %s", got, want)
		}
		if len(s.Periods) != 12 {
			t.Errorf("the contract's periods: got %d, want 12", len(s.Periods))
		}
	})

	e.delivered(t, order, 1, "40", "2026-03-05")
	t.Run("all delivered, after the year", func(t *testing.T) {
		s := e.mustSchedule(t, id, "2027-01-01")
		goods := s.Obligations[0]
		// The last delivery takes the remainder: 1451.61 − 967.74.
		if got, want := rows(goods.Periods), "2026/1 967.74 967.74 0.00 80 EA; 2026/3 483.87 483.87 0.00 40 EA"; got != want {
			t.Errorf("the goods' periods: got %s, want %s", got, want)
		}
		if got, want := sums(goods), "1451.61 = 1451.61 + 0.00 + 0.00"; got != want {
			t.Errorf("the goods' sums: got %s, want %s", got, want)
		}
		if got, want := fmt.Sprintf("%s %s %s", s.RecognizedAmount.Amount, s.PlannedAmount.Amount, s.UnscheduledAmount.Amount), "3000.00 0.00 0.00"; got != want {
			t.Errorf("the contract's sums: got %s, want %s", got, want)
		}
	})
	t.Run("before anything", func(t *testing.T) {
		s := e.mustSchedule(t, id, "2025-12-31")
		if got, want := fmt.Sprintf("%s %s %s", s.RecognizedAmount.Amount, s.PlannedAmount.Amount, s.UnscheduledAmount.Amount), "0.00 3000.00 0.00"; got != want {
			t.Errorf("the contract's sums: got %s, want %s", got, want)
		}
	})
	t.Run("through the API", func(t *testing.T) {
		got, _ := e.Must(200, "GET", path+"/schedule", "", "")
		fields(t, got, map[string]any{"revenue_contract_id": id, "sales_order_id": order, "company_code": "2000", "customer_number": "BP70000001",
			"customer_name": "Northwind Traders", "currency": "EUR"})
		if a := amount(t, got, "transaction_price"); a != "3000.00 EUR" {
			t.Errorf("transaction price: got %s, want 3000.00 EUR", a)
		}
		obs := obligations(t, got)
		fields(t, obs[1], map[string]any{"timing": "over_time", "service_from": "2026-01-01", "service_to": "2026-12-31", "description": "Support for the fiscal year"})
		if a := amount(t, obs[0], "unscheduled_amount"); a != "0.00 EUR" {
			t.Errorf("the goods' unscheduled amount: got %s, want 0.00 EUR", a)
		}
	})
}

// TestScheduleStraightLine: the straight line's shares per fiscal period
// add up to the allocated amount; nothing is recognized before the
// service period, a part on its first day and all of it on its last, for
// service periods of a day, within a period and across periods.
func TestScheduleStraightLine(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name, from, to, want string
	}{
		{"the fiscal year", "2026-01-01", "2026-12-31", line2026},
		{"one day", "2026-06-30", "2026-06-30", "2026/6 1200.00"},
		{"within a period", "2026-02-03", "2026-02-20", "2026/2 1200.00"},
		{"from the middle of a period to the middle of another", "2026-01-17", "2026-03-16", "2026/1 305.09; 2026/2 569.49; 2026/3 325.42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _ := e.contract(t, e.support())
			e.Must(200, "PATCH", contracts+"/"+id, set(overTime(1, c.from, c.to, "1200.00")), `"1"`)
			e.activated(t, id)
			s := e.mustSchedule(t, id, "2027-06-30")
			if got := amounts(s.Obligations[0].Periods); got != c.want {
				t.Errorf("the periods: got %s, want %s", got, c.want)
			}
			if got, want := sums(s.Obligations[0]), "1200.00 = 1200.00 + 0.00 + 0.00"; got != want {
				t.Errorf("after the service period: got %s, want %s", got, want)
			}
			var last string
			for _, day := range []string{c.from, c.to} {
				s := e.mustSchedule(t, id, day)
				o := s.Obligations[0]
				if day == c.from && c.from != c.to && (o.RecognizedAmount.Amount == "0.00" || o.RecognizedAmount.Amount == "1200.00") {
					t.Errorf("recognized on the service period's first day: got %s, want a part of 1200.00", o.RecognizedAmount.Amount)
				}
				last = o.RecognizedAmount.Amount
			}
			if last != "1200.00" {
				t.Errorf("recognized on the service period's last day: got %s, want 1200.00", last)
			}
			before := e.mustSchedule(t, id, "2025-06-30").Obligations[0]
			if got, want := sums(before), "1200.00 = 0.00 + 1200.00 + 0.00"; got != want {
				t.Errorf("before the service period: got %s, want %s", got, want)
			}
		})
	}
}

// satisfiedOn is the JSON that sets the day the obligation is satisfied
// on; "null" removes it.
func satisfiedOn(no int, day string) string {
	if day != "null" {
		day = fmt.Sprintf("%q", day)
	}
	return fmt.Sprintf(`{"obligation_no": %d, "satisfied_on": %s}`, no, day)
}

// TestSatisfiedOn: the revenue accountant names the day a point-in-time
// obligation is satisfied on; the schedule then has the whole allocated
// amount on that day, whatever was delivered; an over-time obligation
// takes no such day (product owner, 2026-11-12; ADR-0068 §8).
func TestSatisfiedOn(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	path := contracts + "/" + id
	got, _ := e.Must(200, "PATCH", path, set(satisfiedOn(1, "2026-04-10")), `"1"`)
	fields(t, obligations(t, got)[0], map[string]any{"timing": "point_in_time", "satisfied_on": "2026-04-10"})
	fields(t, obligations(t, got)[1], map[string]any{"satisfied_on": nil})

	refusals := []struct{ name, body, code, field string }{
		{"over time with the day it is satisfied on", set(`{"obligation_no": 1, "timing": "over_time", "service_from": "2026-01-01", "service_to": "2026-12-31"}`),
			"revenue.satisfied_on_not_allowed", "obligations[0].satisfied_on"},
		{"a day on an over-time obligation", set(overTime(2, "2026-01-01", "2026-12-31", "1200.00"), satisfiedOn(2, "2026-04-10")),
			"revenue.obligation_repeated", "obligations[1].obligation_no"},
		{"a day sent with over time", set(`{"obligation_no": 2, "timing": "over_time", "service_from": "2026-01-01", "service_to": "2026-12-31", "satisfied_on": "2026-04-10"}`),
			"revenue.satisfied_on_not_allowed", "obligations[0].satisfied_on"},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.Must(422, "PATCH", path, c.body, `"2"`)
			if got["code"] != c.code || field(got) != c.field {
				t.Errorf("got %v naming %v, want %s naming %s", got["code"], field(got), c.code, c.field)
			}
		})
	}
	t.Run("removed and set again", func(t *testing.T) {
		got, _ := e.Must(200, "PATCH", path, set(satisfiedOn(1, "null")), `"2"`)
		fields(t, obligations(t, got)[0], map[string]any{"satisfied_on": nil})
		got, _ = e.Must(200, "PATCH", path, set(satisfiedOn(1, "2026-04-10")), `"3"`)
		fields(t, obligations(t, got)[0], map[string]any{"satisfied_on": "2026-04-10"})
	})
	e.activated(t, id)
	e.delivered(t, order, 1, "50", "2026-01-15")
	for _, c := range []struct{ day, want string }{
		{"2026-04-09", "2026/4 1500.00 0.00 1500.00"},
		{"2026-04-10", "2026/4 1500.00 1500.00 0.00"},
	} {
		t.Run("the schedule on "+c.day, func(t *testing.T) {
			s := e.mustSchedule(t, id, c.day)
			if got := rows(s.Obligations[0].Periods); got != c.want {
				t.Errorf("the goods' periods: got %s, want %s", got, c.want)
			}
			if got := s.Obligations[0].UnscheduledAmount.Amount; got != "0.00" {
				t.Errorf("the goods' unscheduled amount: got %s, want 0.00", got)
			}
			if got, ok := s.Obligations[0].SatisfiedOn.Get(); !ok || got.Format(time.DateOnly) != "2026-04-10" {
				t.Errorf("satisfied_on: got %v, want 2026-04-10", got)
			}
		})
	}
}

// TestScheduleRefusals: a draft has no schedule; a day the schedule
// needs a fiscal period for that lies in no fiscal year of the company
// is refused naming the obligation (T81c acceptance).
func TestScheduleRefusals(t *testing.T) {
	e := setup(t)
	t.Run("a draft", func(t *testing.T) {
		id, _ := e.contract(t, e.goods())
		got, _ := e.Must(409, "GET", contracts+"/"+id+"/schedule", "", "")
		if got["code"] != "revenue.invalid_status" {
			t.Errorf("got %v, want revenue.invalid_status", got["code"])
		}
	})
	t.Run("a contract that is not there", func(t *testing.T) {
		e.Must(404, "GET", contracts+"/"+uuid.NewString()+"/schedule", "", "")
	})
	outside := []struct{ name, change, want string }{
		{"a service period that leaves the calendar", overTime(2, "2026-07-01", "2027-06-30", "1200.00"),
			"Obligation 2 (Support for the fiscal year): a day of its service period, 2027-01-01, lies in no fiscal year set up for company 2000"},
		{"a day it is satisfied on outside the calendar", satisfiedOn(1, "2025-12-31"),
			"Obligation 1 (Machined bracket M8): the day it is satisfied on, 2025-12-31, lies in no fiscal year set up for company 2000"},
	}
	for _, c := range outside {
		t.Run(c.name, func(t *testing.T) {
			id, _ := e.contract(t, e.goods(), e.support())
			e.Must(200, "PATCH", contracts+"/"+id, set(c.change), `"1"`)
			e.activated(t, id)
			got, _ := e.Must(422, "GET", contracts+"/"+id+"/schedule", "", "")
			if got["code"] != "revenue.no_fiscal_period" {
				t.Errorf("got %v, want revenue.no_fiscal_period", got["code"])
			}
			if detail, _ := got["detail"].(string); !strings.HasPrefix(detail, c.want) {
				t.Errorf("detail: got %q, want it to start with %q", detail, c.want)
			}
		})
	}
	t.Run("as a problem of the handler", func(t *testing.T) {
		id, _ := e.contract(t, e.goods())
		e.Must(200, "PATCH", contracts+"/"+id, set(satisfiedOn(1, "2031-01-01")), `"1"`)
		e.activated(t, id)
		_, err := e.scheduleAt(t, id, "2026-06-30")
		if p := problem.From(err); p.Code != "revenue.no_fiscal_period" {
			t.Errorf("got %v, want revenue.no_fiscal_period", p)
		}
	})
}
