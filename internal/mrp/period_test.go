package mrp

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// date is a day as dates are held, at midnight UTC.
func date(t *testing.T, v string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, v)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// due is demand as "quantity@day ...".
func due(t *testing.T, v string) []dated {
	t.Helper()
	var out []dated
	for _, f := range strings.Fields(v) {
		q, d, _ := strings.Cut(f, "@")
		out = append(out, dated{day: date(t, d), quantity: money.ParseRat(q)})
	}
	return out
}

// runDay is the tests' run day, a Wednesday: its ISO week runs from
// Monday 23 to Sunday 29 November 2026.
const runDay = "2026-11-25"

// TestPeriodLots (ADR-0084 As built, period lot sizes): each period
// procedure sizes a material's demand by hand-computed figures from the
// run's day, in the base unit, exactly: a lot per period whose demand
// takes the projection (stock and receipts less the demand so far, plus
// the lots before) below the reorder point, of what brings it back
// there, needed by the first day in the period it falls below; bounded
// and rounded as lot for lot, the surplus counting towards the next
// period; overdue demand and a plant already below its reorder point
// needed on the run's day. No period below the reorder point proposes
// nothing.
func TestPeriodLots(t *testing.T) {
	weekly := func(minimum, maximum, rounding *string) LotSizing {
		return LotSizing{Procedure: Weekly, Minimum: minimum, Maximum: maximum, Rounding: rounding}
	}
	for _, c := range []struct {
		name                 string
		lots                 LotSizing
		reorderPoint, supply string
		demand               string
		want                 string
	}{
		{"weekly: requirements in two weeks are two lots", weekly(nil, nil, nil), "0", "0", "30@2026-12-01 20@2026-12-08",
			"30@2026-12-01 20@2026-12-08"},
		{"weekly: requirements of one week, Monday to Sunday, are one lot", weekly(nil, nil, nil), "0", "0", "30@2026-11-30 20@2026-12-06",
			"50@2026-11-30"},
		{"weekly: the demand in any order", weekly(nil, nil, nil), "0", "0", "20@2026-12-08 30@2026-12-01", "30@2026-12-01 20@2026-12-08"},
		{"weekly: needed by the first day the projection falls below", weekly(nil, nil, nil), "0", "25", "20@2026-11-30 30@2026-12-02",
			"25@2026-12-02"},
		{"weekly: the run's own week, overdue demand on the run's day", weekly(nil, nil, nil), "0", "0", "10@2026-11-29 10@2026-11-23",
			"20@2026-11-25"},
		{"weekly: overdue demand of an earlier week on the run's day", weekly(nil, nil, nil), "0", "0", "15@2026-11-02", "15@2026-11-25"},
		{"weekly: below the reorder point before any demand", weekly(nil, nil, nil), "50", "30", "10@2026-12-10", "20@2026-11-25 10@2026-12-10"},
		{"weekly: above the reorder point throughout", weekly(nil, nil, nil), "10", "100", "50@2026-12-01 40@2026-12-08", ""},
		{"weekly: down to the reorder point exactly", weekly(nil, nil, nil), "10", "100", "90@2026-12-01", ""},
		{"weekly: no demand, at the reorder point", weekly(nil, nil, nil), "10", "10", "", ""},
		{"weekly: rounded, the surplus counting next week", weekly(nil, nil, s("10")), "0", "0", "25@2026-12-01 20@2026-12-08",
			"30@2026-12-01 20@2026-12-08"},
		{"weekly: rounded, the surplus covering next week", weekly(nil, nil, s("10")), "0", "0", "25@2026-12-01 5@2026-12-08", "30@2026-12-01"},
		{"weekly: raised to the minimum", weekly(s("40"), nil, nil), "0", "0", "25@2026-12-01 20@2026-12-08", "40@2026-12-01 40@2026-12-08"},
		{"weekly: split by the maximum", weekly(nil, s("20"), nil), "0", "0", "50@2026-12-01", "20@2026-12-01 20@2026-12-01 10@2026-12-01"},
		{"weekly: an ISO week across the year end", weekly(nil, nil, nil), "0", "0", "10@2026-12-31 10@2027-01-03 5@2027-01-04",
			"20@2026-12-31 5@2027-01-04"},
		{"weekly: a fraction of the base unit rounded up to six decimals", weekly(nil, nil, nil), "0", "0", "0.3333333@2026-12-01", "0.333334@2026-12-01"},
		{"daily: a lot per day", LotSizing{Procedure: Daily}, "0", "0", "10@2026-12-01 5@2026-12-01 7@2026-12-02", "15@2026-12-01 7@2026-12-02"},
		{"monthly: a lot per calendar month", LotSizing{Procedure: Monthly}, "0", "0", "10@2026-11-30 10@2026-12-01 10@2026-12-31 10@2027-01-01",
			"10@2026-11-30 20@2026-12-01 10@2027-01-01"},
		{"monthly: the run's month from the run's day", LotSizing{Procedure: Monthly}, "5", "5", "10@2026-11-01", "10@2026-11-25"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lots, err := c.lots.periodLots(money.ParseRat(c.reorderPoint), money.ParseRat(c.supply), due(t, c.demand), date(t, runDay), "MA1")
			if err != nil {
				t.Fatalf("got %v, want lots", err)
			}
			got := make([]string, len(lots))
			for i, x := range lots {
				got[i] = money.PlainQuantity(x.quantity) + "@" + x.due.Format(time.DateOnly)
			}
			if strings.Join(got, " ") != c.want {
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
}

// TestPeriodTooManyLots: daily lots over more than MaxLots days are
// refused, naming the material and its periods; MaxLots are proposed.
func TestPeriodTooManyLots(t *testing.T) {
	demand := func(days int) []dated {
		out := make([]dated, days)
		for i := range out {
			out[i] = dated{day: date(t, runDay).AddDate(0, 0, i), quantity: big.NewRat(1, 1)}
		}
		return out
	}
	daily := LotSizing{Procedure: Daily}
	lots, err := daily.periodLots(new(big.Rat), new(big.Rat), demand(MaxLots), date(t, runDay), "MA1")
	if err != nil {
		t.Fatalf("MaxLots days: got %v, want lots", err)
	}
	if len(lots) != MaxLots {
		t.Errorf("MaxLots days: got %d lots, want %d", len(lots), MaxLots)
	}
	refused := func(name string, err error, detail string) {
		t.Helper()
		var p *problem.Problem
		if !errors.As(err, &p) {
			t.Fatalf("%s: got %v, want a problem", name, err)
		}
		if got := fmt.Sprint(p.Status, " ", p.Code); got != "409 mrp.too_many_lots" {
			t.Errorf("%s: got %s, want 409 mrp.too_many_lots", name, got)
		}
		if !strings.HasPrefix(p.Detail, detail) {
			t.Errorf("%s: got detail %q, want it to start %q", name, p.Detail, detail)
		}
	}
	_, err = daily.periodLots(new(big.Rat), new(big.Rat), demand(MaxLots+1), date(t, runDay), "MA1")
	refused("one day more", err, "Material MA1 needs more than 100 lots over its daily periods")
	_, err = LotSizing{Procedure: Weekly, Maximum: s("0.01")}.periodLots(new(big.Rat), new(big.Rat), due(t, "1.01@2026-12-01"), date(t, runDay), "MA1")
	refused("a period's own lots", err, "Material MA1 needs 101 lots of 0.01")
}

// TestPeriodStart: a day's period starts on the day itself, the Monday of
// its ISO week or the first of its month.
func TestPeriodStart(t *testing.T) {
	for _, c := range []struct{ procedure, day, want string }{
		{Daily, "2026-11-29", "2026-11-29"},
		{Weekly, "2026-11-23", "2026-11-23"}, // a Monday
		{Weekly, "2026-11-25", "2026-11-23"},
		{Weekly, "2026-11-29", "2026-11-23"}, // a Sunday
		{Weekly, "2027-01-01", "2026-12-28"},
		{Monthly, "2026-12-31", "2026-12-01"},
		{Monthly, "2026-12-01", "2026-12-01"},
	} {
		t.Run(c.procedure+" "+c.day, func(t *testing.T) {
			if got := periodStart(c.procedure, date(t, c.day)).Format(time.DateOnly); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
