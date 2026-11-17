package collections_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// charged is company 2000's dunning with a procedure (T76d): 5 grace
// days, no notice below 120.00 EUR, interest at 9% a year; level 1
// charges 5.00 and no interest, level 2 15.00 and interest.
func charged(doc *config.Config) {
	dunning(doc)
	d := &doc.Company("2000").Dunning
	d.GraceDays, d.MinimumAmount, d.InterestRatePercent = 5, "120.00", "9.000"
	d.Levels[0].Charge = "5.00"
	d.Levels[1].Charge, d.Levels[1].Interest = "15.00", true
}

// fields are some of an object's fields as "name value", amounts as
// "amount currency".
func fields(t *testing.T, m map[string]any, names ...string) string {
	t.Helper()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		v := m[n]
		if o, ok := v.(map[string]any); ok {
			v = fmt.Sprintf("%v %v", o["amount"], o["currency"])
		}
		out += fmt.Sprintf("%s %v", n, v)
	}
	return out
}

// TestDunningCharges (T76d acceptance): with the company's procedure, a
// run leaves out an item overdue by no more than the grace days and a
// customer whose notice would be below the minimum amount, counting it;
// a notice is charged its level's charge and, at a level that charges
// interest, interest on each item at the company's rate for its days
// overdue, rounded half away from zero; the run keeps the procedure and
// adds up the charges, and a confirmation publishes them. The items of a
// notice below the minimum are not recorded, so the next run proposes
// them from level 0.
func TestDunningCharges(t *testing.T) {
	e := setupWith(t, charged)
	e.story(t)
	// Northwind: CI-A, 100.00, is 91 days overdue; CI-B, 5 days, is within the grace days; so 100.00, below 120.00.
	// Southwind: 200.00 USD booked at 160.00 EUR, 61 days overdue, at level 1: 5.00 and no interest.
	first, tag := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	want := "notice_count 1, item_count 1, overdue_amount 160.00 EUR, charge_amount 5.00 EUR, interest_amount 0.00 EUR, total_amount 165.00 EUR, " +
		"grace_days 5, minimum_amount 120.00 EUR, interest_rate_percent 9.000, below_minimum_notice_count 1, below_minimum_amount 100.00 EUR"
	names := []string{"notice_count", "item_count", "overdue_amount", "charge_amount", "interest_amount", "total_amount", "grace_days",
		"minimum_amount", "interest_rate_percent", "below_minimum_notice_count", "below_minimum_amount"}
	if got := fields(t, first, names...); got != want {
		t.Errorf("the first run: got %s, want %s", got, want)
	}
	id := apitest.String(t, first, "id")
	if got := e.proposed(t, id); got != "CI-USD 0->1" {
		t.Errorf("proposed: got %q, want Southwind's item alone", got)
	}
	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	south := objects(t, page, "items")[0]
	if got, want := fields(t, south, "level", "overdue_amount", "charge_amount", "interest_amount", "total_amount"),
		"level 1, overdue_amount 160.00 EUR, charge_amount 5.00 EUR, interest_amount 0.00 EUR, total_amount 165.00 EUR"; got != want {
		t.Errorf("Southwind's notice: got %s, want %s", got, want)
	}
	e.confirm(t, id, tag)
	issued := e.issued(t)
	if len(issued) != 1 {
		t.Fatalf("DunningNoticeIssued events: got %d, want 1", len(issued))
	}
	if got, want := fields(t, issued[0], "charge_amount", "interest_amount", "total_amount"),
		"charge_amount 5.00 EUR, interest_amount 0.00 EUR, total_amount 165.00 EUR"; got != want {
		t.Errorf("the notice's event: got %s, want %s", got, want)
	}

	// A month later Southwind's item, 91 days overdue, reaches level 2: 15.00 and 160.00 × 9% × 91 ÷ 365 = 3.590, 3.59.
	// Northwind's 290.00 is above the minimum, at level 1 from level 0: its first notice was below the minimum and recorded nothing.
	// Eastwind's 90.00, 15 days overdue, is below it.
	second, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, "2026-07-30"), "")
	want = "notice_count 2, item_count 6, overdue_amount 450.00 EUR, charge_amount 20.00 EUR, interest_amount 3.59 EUR, total_amount 473.59 EUR, " +
		"grace_days 5, minimum_amount 120.00 EUR, interest_rate_percent 9.000, below_minimum_notice_count 1, below_minimum_amount 90.00 EUR"
	if got := fields(t, second, names...); got != want {
		t.Errorf("the second run: got %s, want %s", got, want)
	}
	id = apitest.String(t, second, "id")
	if got, want := e.proposed(t, id), "CI-USD 1->2, CI-A 0->1, CI-B 0->1, CI-C 0->1, CI-D 0->1, MANUAL 0->1"; got != want {
		t.Errorf("proposed: got %q, want %q", got, want)
	}
	page, _ = e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	notices := objects(t, page, "items")
	for _, c := range []struct {
		name   string
		notice map[string]any
		want   string
	}{
		{"Southwind's", notices[0], "level 2, overdue_amount 160.00 EUR, charge_amount 15.00 EUR, interest_amount 3.59 EUR, total_amount 178.59 EUR"},
		{"Northwind's", notices[1], "level 1, overdue_amount 290.00 EUR, charge_amount 5.00 EUR, interest_amount 0.00 EUR, total_amount 295.00 EUR"},
	} {
		t.Run(c.name+" notice", func(t *testing.T) {
			if got := fields(t, c.notice, "level", "overdue_amount", "charge_amount", "interest_amount", "total_amount"); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	if got := fields(t, objects(t, notices[0], "items")[0], "days_overdue", "level", "interest_amount"); got != "days_overdue 91, level 2, interest_amount 3.59 EUR" {
		t.Errorf("Southwind's item: got %s, want 91 days at level 2 with 3.59 EUR of interest", got)
	}
}

// TestDunningWithoutProcedure: a run of a company with levels alone keeps
// no rate, gives no grace and charges nothing (T76d acceptance: as
// before).
func TestDunningWithoutProcedure(t *testing.T) {
	e := setup(t)
	e.story(t)
	got, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	want := "charge_amount 0.00 EUR, interest_amount 0.00 EUR, total_amount 310.00 EUR, grace_days 0, minimum_amount 0.00 EUR, " +
		"interest_rate_percent <nil>, below_minimum_notice_count 0, below_minimum_amount 0.00 EUR"
	if g := fields(t, got, "charge_amount", "interest_amount", "total_amount", "grace_days", "minimum_amount", "interest_rate_percent",
		"below_minimum_notice_count", "below_minimum_amount"); g != want {
		t.Errorf("got %s, want %s", g, want)
	}
}

// interestEverywhere is charged with interest from level 1 at the highest
// rate, for amounts out of range.
func interestEverywhere(doc *config.Config) {
	charged(doc)
	d := &doc.Company("2000").Dunning
	d.InterestRatePercent, d.MinimumAmount, d.Levels[0].Interest = "99.999", "", true
}

// TestChargesOutOfRange: interest that an amount cannot hold, and a
// notice whose total cannot, are refused (422
// collections.amount_too_large), as the overdue amounts are.
func TestChargesOutOfRange(t *testing.T) {
	e := setupWith(t, interestEverywhere)
	e.story(t)
	for _, c := range []struct{ name, key string }{
		// 2^62 × 99.999% × about 27,000 days ÷ 365: the interest alone is out of range.
		{"interest", "2100-12-31"},
		// 2^62 × 99.999% × 395 days ÷ 365 is about 1.08 × 2^62: in range, and with the item's 2^62 not.
		{"a notice's total", "2027-04-30"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := endpoints(t)
			h.OpenItems = huge{customers: []uuid.UUID{e.north}}
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.ProposeRun(ctx, collections.RunInput{LegalEntityID: e.DE, KeyDate: date(t, c.key)})
				return err
			})
			if p := problem.From(err); p.Code != "collections.amount_too_large" {
				t.Errorf("got %v, want collections.amount_too_large", err)
			}
		})
	}
}
