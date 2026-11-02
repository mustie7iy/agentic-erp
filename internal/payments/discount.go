package payments

import (
	"time"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// deductionsOn are the cash discount an item owing amount (in the sign it
// is paid in) takes when paid on the date, by the terms it was posted with
// (ledger.ItemTerms.DiscountOn), and the withholding tax deducted from
// what it pays after the discount (T46f), as the ledger posts it
// (ledger.PaymentWithholding). The proposer and the liquidity forecast
// (CashPosition.Payables) both read them here.
func deductionsOn(terms *ledger.ItemTerms, withholding []ledger.Withholding, amount money.Minor, on time.Time) (discount, withheld money.Minor, err error) {
	if discount, err = terms.DiscountOn(amount, on); err != nil {
		return 0, 0, err
	}
	if withheld, err = ledger.PaymentWithholding(withholding, amount, discount); err != nil {
		return 0, 0, err
	}
	return discount, withheld, nil
}
