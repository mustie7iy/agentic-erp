package ledger

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Totals of items out of the bigint range are refused, never wrapped: a
// settlement's bank line would carry the wrapped total, and a clearing
// whose sums wrapped to zero would pass as netting to zero.
func TestItemTotalsOutOfRange(t *testing.T) {
	legalEntity, ledgerID, partner := uuid.New(), uuid.New(), uuid.New()
	top := money.Minor(math.MaxInt64)
	huge := item{found: true, open: true, recon: true, legalEntity: legalEntity, ledger: ledgerID, account: uuid.New(), partner: &partner,
		tc: top, lc: top, gc: top, currency: "EUR"}
	small := huge
	small.tc, small.lc, small.gc = 2, 2, 2
	items := []item{huge, huge, small} // 2^64: zero when wrapped

	_, _, _, _, err := settledItemLines(items, Settlement{Currency: "EUR"}, legalEntity)
	if code := problemCode(err); code != "ledger.amount_too_large" {
		t.Errorf("settlement: got %v, want ledger.amount_too_large", err)
	}
	_, err = clearingLines(items, legalEntity, nil)
	if code := problemCode(err); code != "ledger.clearing_not_balanced" {
		t.Errorf("clearing: got %v, want ledger.clearing_not_balanced", err)
	}
}

// The exchange difference of a settlement is checked: items booked at
// 8.5e18 local minor units settled by a bank line of -8e18 differ by more
// than an amount holds, which wrapped into the fx line before.
func TestExchangeDifferenceOutOfRange(t *testing.T) {
	cases := map[string]struct{ lc, gc, bankLC, bankGC money.Minor }{
		"local":    {8_500_000_000_000_000_000, 0, -8_000_000_000_000_000_000, 0},
		"group":    {0, -8_500_000_000_000_000_000, 0, 8_000_000_000_000_000_000},
		"in range": {5, 7, 3, 2},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bank := settlementBank{lc: c.bankLC, gc: c.bankGC}
			lc, gc, err := exchangeDifferences(c.lc, c.gc, bank)
			if name == "in range" {
				if err != nil || lc != 2 || gc != 5 {
					t.Errorf("got %d, %d, %v, want 2, 5, nil", lc, gc, err)
				}
				return
			}
			if code := problemCode(err); code != "ledger.amount_too_large" {
				t.Errorf("got %v, want ledger.amount_too_large", err)
			}
			// Refused before the lines read the account determination.
			if _, err := exchangeDifferenceLines(context.Background(), uuid.Nil, c.lc, c.gc, bank, nil); problemCode(err) != "ledger.amount_too_large" {
				t.Errorf("lines: got %v, want ledger.amount_too_large", err)
			}
		})
	}
}

// A conversion to int64's most negative value is too large to post: it has
// no positive counterpart, so its reversal or clearing offset would keep
// the sign, and it rendered as "--92233720368547758.-8".
func TestConvertToMostNegative(t *testing.T) {
	if got, err := convert(-1<<62, "2", 2, 2); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("got %d, %v, want ledger.amount_too_large", got, err)
	}
}

// A rate the database never renders is an internal error, not the
// caller's amount.
func TestConvertInvalidRate(t *testing.T) {
	_, err := convert(100, "1e5", 2, 2)
	if err == nil || problemCode(err) != "" {
		t.Errorf("got %v, want an error that is not a problem", err)
	}
}

func problemCode(err error) string {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return ""
	}
	return p.Code
}
