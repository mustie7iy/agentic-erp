package revenue

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// months is a calendar whose fiscal periods are the calendar months of
// every year.
type months struct{}

func (months) Period(_ context.Context, _ uuid.UUID, day time.Time) (ledger.FiscalPeriod, error) {
	start := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	return ledger.FiscalPeriod{FiscalYear: int16(day.Year()), Number: int16(day.Month()), Start: start, End: start.AddDate(0, 1, -1)}, nil
}

func (m months) Periods(ctx context.Context, le uuid.UUID, year int16) ([]ledger.FiscalPeriod, error) {
	out := make([]ledger.FiscalPeriod, 12)
	for i := range out {
		out[i], _ = m.Period(ctx, le, time.Date(int(year), time.Month(i+1), 1, 0, 0, 0, 0, time.UTC))
	}
	return out, nil
}

func date(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

// TestStraightLineAddsUp: for 300 fixed amounts and service periods, of
// a day to three years, the periods' amounts add up to the allocated
// amount to the minor unit and their days to the service period's; what
// is recognized never falls from one day to the next and is the whole
// amount on the last day (T81c acceptance).
func TestStraightLineAddsUp(t *testing.T) {
	ctx := context.Background()
	for i := range 300 {
		amount := money.Minor((i*7919+1)%1000003) * money.Minor(1+i%7)
		from := date(2026, 1+i%12, 1+i%28)
		to := from.AddDate(0, 0, (i*37)%1096)
		o := Obligation{No: 1, Timing: OverTime, ServiceFrom: &from, ServiceTo: &to, Allocated: &amount}
		t.Run(fmt.Sprintf("%d from %s to %s", amount, from.Format(time.DateOnly), to.Format(time.DateOnly)), func(t *testing.T) {
			cal := &calendar{source: months{}}
			rows, err := cal.straightLine(ctx, o, to)
			if err != nil {
				t.Fatal(err)
			}
			var sum, recognized money.Minor
			var days int64
			for _, r := range rows {
				sum, recognized, days = sum+r.Amount, recognized+r.Recognized, days+int64(r.Days)
			}
			if sum != amount {
				t.Errorf("the periods' amounts: got %d, want %d", sum, amount)
			}
			if recognized != amount {
				t.Errorf("recognized on the last day: got %d, want %d", recognized, amount)
			}
			if want := daysBetween(from, to); days != want {
				t.Errorf("the periods' days: got %d, want %d", days, want)
			}
			last := money.Minor(0)
			for day := from.AddDate(0, 0, -1); !day.After(to); day = day.AddDate(0, 0, 1+int(days)/40) {
				s, err := cal.obligation(ctx, o, sales.OrderItem{}, day)
				if err != nil {
					t.Fatal(err)
				}
				if s.Recognized < last {
					t.Errorf("recognized on %s: got %d, want at least %d, the day before's", day.Format(time.DateOnly), s.Recognized, last)
				}
				if s.Recognized+s.Planned != amount || s.Unscheduled != 0 {
					t.Errorf("on %s: got recognized %d, planned %d, unscheduled %d, want them to add up to %d with nothing unscheduled",
						day.Format(time.DateOnly), s.Recognized, s.Planned, s.Unscheduled, amount)
				}
				last = s.Recognized
			}
		})
	}
}

// TestDeliveriesAddUp: for 200 fixed amounts and quantities delivered in
// parts, what the deliveries schedule and what is left unscheduled add
// up to the allocated amount after every delivery, and the delivery that
// completes the item leaves nothing (T81c acceptance).
func TestDeliveriesAddUp(t *testing.T) {
	ctx := context.Background()
	for i := range 200 {
		amount := money.Minor((i*104729 + 3) % 999983)
		parts := 1 + i%6
		item := sales.OrderItem{Item: sales.Item{Quantity: fmt.Sprintf("%d.%03d", parts*(3+i%5), 0), Unit: "EA"}}
		o := Obligation{No: 1, Timing: PointInTime, Allocated: &amount}
		t.Run(fmt.Sprintf("%d in %d parts", amount, parts), func(t *testing.T) {
			for k := 1; k <= parts; k++ {
				item.Deliveries = append(item.Deliveries, sales.ItemDelivery{Date: date(2026, k, 10+k), Quantity: fmt.Sprintf("%d", 3+i%5)})
				s, err := (&calendar{source: months{}}).obligation(ctx, o, item, date(2026, 12, 31))
				if err != nil {
					t.Fatal(err)
				}
				if s.Recognized+s.Unscheduled != amount || s.Planned != 0 {
					t.Errorf("after delivery %d: got recognized %d, planned %d, unscheduled %d, want them to add up to %d with nothing planned",
						k, s.Recognized, s.Planned, s.Unscheduled, amount)
				}
				if k == parts && s.Unscheduled != 0 {
					t.Errorf("after the last delivery: got %d unscheduled, want 0", s.Unscheduled)
				}
				if len(s.Periods) != k {
					t.Errorf("after delivery %d: got %d periods, want %d", k, len(s.Periods), k)
				}
			}
		})
	}
}

// TestRunTotals: a run's totals are its contracts' amounts added up;
// sums beyond what an amount holds are refused, not wrapped around.
func TestRunTotals(t *testing.T) {
	run, err := totalled(Run{}, []money.Minor{100, 250}, []money.Minor{40, 300}, []money.Minor{60, 0}, []money.Minor{0, 50}, []money.Minor{60, -50})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want money.Minor
	}{{"recognized", run.Recognized, 350}, {"billed", run.Billed, 340}, {"contract assets", run.Asset, 60}, {"contract liabilities", run.Liability, 50},
		{"revenue moved", run.Revenue, 10}} {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}
	_, err = totalled(Run{}, []money.Minor{math.MaxInt64, 1}, nil, nil, nil, nil)
	if p := problem.From(err); p.Code != "revenue.amount_too_large" {
		t.Errorf("sums beyond an amount: got %v, want revenue.amount_too_large", p)
	}
}
