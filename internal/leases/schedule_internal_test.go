package leases

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// lease is the terms and payments of a lease from 2026-01-01 of n
// intervals of the months, each paying amount.
func lease(months, n int, timing string, amount money.Minor) (Terms, []Payment) {
	t := Terms{Commencement: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), TermMonths: months * n, Interval: months, Timing: timing}
	return t, t.Payments(amount)
}

// lines is the schedule's lines as "opening/payment/interest/closing
// depreciation carrying", one per line.
func lines(s Schedule) string {
	var out []string
	for _, l := range s.Lines {
		out = append(out, fmt.Sprintf("%d/%d/%d/%d %d %d", l.Opening, l.Payment, l.Interest, l.Closing, l.Depreciation, l.RightOfUseCarrying))
	}
	return strings.Join(out, ", ")
}

// TestScheduleByHand (T88b; ADR-0073 §8): three yearly payments of 100.00
// at 10 percent, worked by hand. In arrears the liability is
// 100/1.1 + 100/1.1² + 100/1.1³ = 248.685…, rounded 248.69, and is the
// right-of-use asset. In advance the first payment, on the commencement
// date, is not in the liability, 100/1.1 + 100/1.1² = 173.553…, rounded
// 173.55, and the right-of-use asset is 273.55; the first interval bears
// interest on 173.55 alone. Each closing liability is the exact present
// value of what is still to come, rounded once; the depreciation's
// shares add up to the right-of-use asset, the cent over going to the
// earlier lines.
func TestScheduleByHand(t *testing.T) {
	cases := []struct {
		timing, want, lines string
		dates               string
	}{
		{Arrears, "24869 0 24869", "24869/10000/2486/17355 8290 16579, 17355/10000/1736/9091 8290 8289, 9091/10000/909/0 8289 0",
			"2026-01-01 2026-12-31 2028-12-31"},
		{Advance, "17355 10000 27355", "27355/10000/1736/19091 9119 18236, 19091/10000/909/10000 9118 9118, 10000/10000/0/0 9118 0",
			"2026-01-01 2026-12-31 2028-01-01"},
	}
	for _, c := range cases {
		t.Run(c.timing, func(t *testing.T) {
			tm, payments := lease(12, 3, c.timing, 10000)
			s := tm.Schedule(payments, big.NewRat(10, 1))
			if got := fmt.Sprintf("%d %d %d", s.Liability, s.CommencementPayment, s.RightOfUse); got != c.want {
				t.Errorf("liability, commencement payment, right-of-use: got %s, want %s", got, c.want)
			}
			if got := lines(s); got != c.lines {
				t.Errorf("lines: got %s, want %s", got, c.lines)
			}
			got := fmt.Sprintf("%s %s %s", s.Lines[0].Start.Format(time.DateOnly), s.Lines[0].End.Format(time.DateOnly), s.Lines[2].PaymentDate.Format(time.DateOnly))
			if got != c.dates {
				t.Errorf("first interval and last payment date: got %s, want %s", got, c.dates)
			}
		})
	}
}

// presentValue is Σ amount / (1 + r)^k for k from first to last: the
// annuity the liability at commencement is, computed apart from the
// schedule's recursion.
func presentValue(amount money.Minor, r *big.Rat, first, last int) *big.Rat {
	growth := new(big.Rat).Add(big.NewRat(1, 1), r)
	sum, factor := new(big.Rat), big.NewRat(1, 1)
	for k := 1; k <= last; k++ {
		factor.Quo(factor, growth)
		if k >= first {
			sum.Add(sum, new(big.Rat).Mul(factor, new(big.Rat).SetInt64(int64(amount))))
		}
	}
	return sum
}

// TestScheduleOf360Periods (T88b; ADR-0073 §8): thirty years of monthly
// payments of 1,000.00 at 6 percent, 0.5 percent a month. The liability is
// the annuity's present value (166,791.61 in arrears, the familiar
// mortgage figure); every line's interest is its closing liability less
// its opening plus its payment and lies within a cent of the period's
// rate on the balance outstanding during it; the last closing liability
// is zero with no remainder absorbed, the interest adds up to the
// payments less the right-of-use asset, and the depreciation to the
// right-of-use asset.
func TestScheduleOf360Periods(t *testing.T) {
	rate := big.NewRat(6, 1)
	monthly := big.NewRat(5, 1000)
	for _, timing := range []string{Arrears, Advance} {
		t.Run(timing, func(t *testing.T) {
			tm, payments := lease(1, 360, timing, 100000)
			s := tm.Schedule(payments, rate)
			first, last := 1, 360
			if timing == Advance {
				last = 359
			}
			want, _ := money.Round(presentValue(100000, monthly, first, last), 0) // well in range
			if s.Liability != want {
				t.Errorf("liability: got %d, want %d", s.Liability, want)
			}
			if timing == Arrears && s.Liability != 16679161 {
				t.Errorf("liability: got %d, want 16679161, 166,791.61", s.Liability)
			}
			var interest, depreciation money.Minor
			for i, l := range s.Lines {
				if l.Interest != l.Closing-l.Opening+l.Payment {
					t.Fatalf("line %d: interest %d is not closing %d - opening %d + payment %d", i+1, l.Interest, l.Closing, l.Opening, l.Payment)
				}
				outstanding := l.Opening
				if timing == Advance {
					outstanding -= l.Payment
				}
				exact := new(big.Rat).Mul(monthly, new(big.Rat).SetInt64(int64(outstanding)))
				f, _ := new(big.Rat).Sub(exact, new(big.Rat).SetInt64(int64(l.Interest))).Float64()
				if math.Abs(f) > 1.5 {
					t.Fatalf("line %d: interest %d is %v from the rate on %d", i+1, l.Interest, f, outstanding)
				}
				interest += l.Interest
				depreciation += l.Depreciation
			}
			if got := s.Lines[359].Closing; got != 0 {
				t.Errorf("last closing liability: got %d, want 0", got)
			}
			if got, want := interest, money.Minor(36000000)-s.RightOfUse; got != want {
				t.Errorf("interest: got %d, want %d, the payments less the right-of-use asset", got, want)
			}
			if depreciation != s.RightOfUse || s.Lines[359].RightOfUseCarrying != 0 {
				t.Errorf("depreciation: got %d ending at %d, want %d ending at 0", depreciation, s.Lines[359].RightOfUseCarrying, s.RightOfUse)
			}
		})
	}
}

// TestRunTotals (T88c): a run's totals add up its postings' interest and
// depreciation, and sums beyond an amount are refused.
func TestRunTotals(t *testing.T) {
	interest, depreciation, err := runTotals([]RunPosting{{Interest: 5, Depreciation: 7}, {Interest: -1, Depreciation: 3}})
	if got := fmt.Sprint(int64(interest), " ", int64(depreciation), " ", err); got != "4 10 <nil>" {
		t.Errorf("totals: got %s, want 4 10 <nil>", got)
	}
	for name, p := range map[string]RunPosting{"interest": {Interest: math.MaxInt64}, "depreciation": {Depreciation: math.MaxInt64}} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runTotals([]RunPosting{p, p}); problem.From(err).Code != "leases.amount_too_large" {
				t.Errorf("got %v, want leases.amount_too_large", err)
			}
		})
	}
}
