package ledger

import (
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
