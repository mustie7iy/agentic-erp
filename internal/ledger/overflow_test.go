package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Line amounts whose total is out of the bigint range are refused as too
// large: before, a total that wrapped to zero passed as balanced and failed
// at commit as an invalid value, and one that wrapped elsewhere was refused
// as unbalanced by a wrong difference.
func TestPostingTotalOutOfRange(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	const largest = "92233720368547758.07" // 2^63-1 cents
	for name, last := range map[string]string{"wraps to zero": "0.02", "wraps to a small difference": "-0.02"} {
		t.Run(name, func(t *testing.T) {
			e.Problem(422, "ledger.amount_too_large", "POST", "/v1/journal-entries",
				entry(e.de, "2026-03-15", "EUR", line{bank, largest, ""}, line{bank, largest, ""}, line{revenue, last, ""}), "")
		})
	}
	// Terms that net to zero post, whatever their order.
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, largest, ""}, line{revenue, largest, ""},
		line{bank, "-" + largest, ""}, line{revenue, "-" + largest, ""}), "")
}

// The debits of a fiscal year are summed exactly: accounts whose debits
// are each in range but add up to more fail the totals instead of wrapping.
func TestTotalsDebitsOutOfRange(t *testing.T) {
	e := setup(t)
	bank, expense, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "400000")
	const largest = "92233720368547758.07" // 2^63-1 cents
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, largest, ""}, line{revenue, "-" + largest, ""}), "")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-16", "EUR", line{expense, largest, ""}, line{bank, "-" + largest, ""}), "")
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := ledger.Reports{}.Totals(ctx, e.de, 2026)
		return err
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("totals: got %v, want %v", err, money.ErrOverflow)
	}
}
