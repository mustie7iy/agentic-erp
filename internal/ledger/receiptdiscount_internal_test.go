package ledger

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDiscountsOutOfRange: discounts each in range whose total is not are
// refused rather than wrapped, and so is a discount line whose amount at
// the receipt's rate is out of range.
func TestDiscountsOutOfRange(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	const near = money.Minor(9_000_000_000_000_000_000)
	a, b := ItemRef{FiscalYear: 2026, EntryID: uuid.UUID{1}, LineNo: 1}, ItemRef{FiscalYear: 2026, EntryID: uuid.UUID{2}, LineNo: 1}
	terms := &ItemTerms{BaselineDate: day, NetDays: 30, Discounts: []Discount{{Days: 10, Percent: "99"}}}
	_, _, err := discountsOf(ReceiptApplication{CashDiscounts: []bool{true, true}, DiscountDate: day}, []item{{ref: a, tc: near}, {ref: b, tc: near}},
		map[ItemRef]*ItemTerms{a: terms, b: terms}, nil)
	if p := problem.From(err); p == nil || p.Code != "ledger.amount_too_large" {
		t.Errorf("discounts adding up beyond an amount: got %v, want ledger.amount_too_large", err)
	}
	code := "S5"
	var app application
	err = app.addDiscountLines(item{tc: -1, lc: -near}, uuid.UUID{9}, []Line{{AccountID: uuid.UUID{8}, TC: 2, TaxCode: &code}})
	if p := problem.From(err); p == nil || p.Code != "ledger.amount_too_large" {
		t.Errorf("a line beyond an amount at the receipt's rate: got %v, want ledger.amount_too_large", err)
	}
}
