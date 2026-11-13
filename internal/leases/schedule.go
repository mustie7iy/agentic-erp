package leases

import (
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Payment is a row of leases.payment: a payment of a contract's
// schedule, numbered from 1 in date order, due on Due, in minor units of
// the contract's currency.
type Payment struct {
	No     int32       `json:"payment_no"`
	Due    time.Time   `json:"due_date"`
	Amount money.Minor `json:"amount"`
}

// Terms are what a contract's payment schedule follows from: the
// commencement date, the term in months, the months of an interval and
// whether payments are due at its start or its end.
type Terms struct {
	Commencement time.Time
	TermMonths   int
	Interval     int // months
	Timing       string
}

// intervals is the number of intervals, and payments, of the term.
func (t Terms) intervals() int { return t.TermMonths / t.Interval }

// start is the first day of interval i, from 0: i intervals after the
// commencement date, on its day of the month or the month's last day
// when it has fewer, counted from the commencement date and not from the
// interval before, so the days do not drift (ADR-0073 §4).
func (t Terms) start(i int) time.Time { return clock.AddMonths(t.Commencement, i*t.Interval) }

// End is the term's last day: the day before the date the term's months
// after the commencement date.
func (t Terms) End() time.Time { return t.start(t.intervals()).AddDate(0, 0, -1) }

// Payments are the contract's payments, one per interval, each of the
// amount: due on the interval's first day in advance, on its last day,
// the day before the next interval starts, in arrears (ADR-0073 §4).
func (t Terms) Payments(amount money.Minor) []Payment {
	out := make([]Payment, t.intervals())
	for i := range out {
		due := t.start(i)
		if t.Timing == Arrears {
			due = t.start(i+1).AddDate(0, 0, -1)
		}
		out[i] = Payment{No: int32(i + 1), Due: due, Amount: amount}
	}
	return out
}
