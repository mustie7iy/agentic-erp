package leases

import (
	"math/big"
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

// Line is a line of a contract's schedule (ADR-0073 §8): one payment
// interval, numbered as its payment, with the liability at its start and
// end, the payment due in it and the interest, and the right-of-use
// asset's depreciation and carrying amount at its end, in minor units of
// the contract's currency.
type Line struct {
	No                 int32
	Start, End         time.Time
	PaymentDate        time.Time
	Opening, Payment   money.Minor
	Interest, Closing  money.Minor
	Depreciation       money.Minor
	RightOfUseCarrying money.Minor
}

// Schedule is a contract's schedule: the lease liability at commencement,
// a payment due on the commencement date, the right-of-use asset, which
// is both, and the lines.
type Schedule struct {
	Liability, CommencementPayment, RightOfUse money.Minor
	Lines                                      []Line
}

// perYear is the number of payments a year, by which the annual rate is
// divided into a period's (default 6).
func (t Terms) perYear() int { return 12 / t.Interval }

// Schedule computes the contract's schedule from its payments, one per
// interval in order, and its annual discount rate in percent (ADR-0073
// §8; IFRS 16 as §2's statements 2 to 5 take it, not yet confirmed). The
// period's rate r is the annual rate divided by the payments a year
// (default 6). From the last interval back, the exact present value at
// each interval's end of the payments still to come is a rational:
// (that of the next interval's end + its payment) / (1 + r) in arrears,
// its payment + that / (1 + r) in advance, where the payment due at an
// interval's start is still to come at the end of the interval before.
// Each is rounded half away from zero to the minor unit once: the closing
// liabilities, the last zero. The present value at commencement, rounded,
// is the first opening liability: in advance it holds the payment due on
// the commencement date, which is not part of the lease liability and is
// added to the right-of-use asset (default 6); the right-of-use asset is
// that opening liability. A line's interest is its closing liability less
// its opening plus its payment, so no rounding difference is carried
// forward. The depreciation is the right-of-use asset on a straight line,
// shared out with money.Allocate so the shares add up to it exactly.
func (t Terms) Schedule(payments []Payment, annualRate *big.Rat) Schedule {
	growth := new(big.Rat).Mul(annualRate, big.NewRat(int64(t.Interval), 1200))
	growth.Add(growth, big.NewRat(1, 1))
	n := len(payments)
	exact := make([]*big.Rat, n+1)
	exact[n] = new(big.Rat)
	for k := n; k >= 1; k-- {
		p := new(big.Rat).SetInt64(int64(payments[k-1].Amount))
		later := new(big.Rat).Quo(exact[k], growth)
		if t.Timing == Arrears {
			exact[k-1] = later.Add(later, new(big.Rat).Quo(p, growth))
		} else {
			exact[k-1] = later.Add(later, p)
		}
	}
	balances := make([]money.Minor, n+1)
	for k, x := range exact {
		// A present value at a positive rate is not above the payments' sum,
		// which a contract holds within an amount's range (Create), so it
		// rounds in range.
		balances[k], _ = money.Round(x, 0)
	}
	s := Schedule{RightOfUse: balances[0], Liability: balances[0]}
	if t.Timing == Advance {
		s.CommencementPayment = payments[0].Amount
		s.Liability -= s.CommencementPayment
	}
	shares := make([]*big.Rat, n)
	for i := range shares {
		shares[i] = big.NewRat(int64(s.RightOfUse), int64(n))
	}
	// The shares add up to the right-of-use asset, an amount in range, so
	// neither their sum nor one of them is out of range.
	depreciation, _ := money.Allocate(shares)
	s.Lines = make([]Line, n)
	carrying := s.RightOfUse
	for k := range n {
		carrying -= depreciation[k]
		s.Lines[k] = Line{No: payments[k].No, Start: t.start(k), End: t.start(k+1).AddDate(0, 0, -1), PaymentDate: payments[k].Due,
			Opening: balances[k], Payment: payments[k].Amount, Interest: balances[k+1] - balances[k] + payments[k].Amount, Closing: balances[k+1],
			Depreciation: depreciation[k], RightOfUseCarrying: carrying}
	}
	return s
}
