package receivables

import (
	"fmt"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Cash discount granted (ADR-0063 As built, cash discount granted; the
// product owner's answer of 2026-11-21): an invoice an incoming payment
// applies may take the cash discount its payment terms grant for a payment
// received within the discount period (items[].cash_discount), when the
// company's country reduces the output tax on it. The ledger clears the
// invoice with the discount and corrects the output tax of each of its tax
// items in proportion, in the emirate each named
// (ledger.ReceiptApplication.CashDiscounts), so the VAT 201 reports the
// reduced tax.

// discountTaxCountry is the one country whose companies grant their
// customers a cash discount that reduces the output tax: the United Arab
// Emirates (the product owner's answer of 2026-11-21, as T36d's
// correction reduces input tax on the supplier side). A Canadian cash
// discount makes no GST/HST adjustment (Excise Tax Act s.161, confirmed by
// the tax specialist, Musto Ibrahim, on 2026-11-21): what a Canadian
// customer takes off stays a payment difference without a tax code (T75f,
// T75o), as it does in every other country until its tax specialist
// answers. A company's country decides, as its tax codes and its returns
// are its country's (the ledger's taxDocumentCountry likewise).
const discountTaxCountry = "AE"

// cashDiscounts are the cash discounts the request asks the items to take,
// as the ledger takes them (nil for none), in a company of the country
// that grants them; elsewhere one asked for is refused, 422
// receivables.cash_discount_not_granted naming items[i].cash_discount.
func cashDiscounts(le tenancy.Entity, asked []bool) ([]bool, error) {
	i := slices.Index(asked, true)
	switch {
	case i < 0:
		return nil, nil
	case le.CountryCode != discountTaxCountry:
		return nil, discountNotGranted(i, fmt.Sprintf("Company %s, of country %s, grants no cash discount: what the customer takes off is a payment "+
			"difference without tax (ADR-0063, cash discount granted), within the payment tolerance or with a residual's reason.", le.Code, le.CountryCode))
	}
	return asked, nil
}

// noCashDiscount refuses a cash discount asked of an application of a
// credit on account (T75g): its items were not paid by a receipt within
// their discount period, so they take none.
func noCashDiscount(asked []bool) error {
	if i := slices.Index(asked, true); i >= 0 {
		return discountNotGranted(i, "An application of a credit on account grants no cash discount: the discount is taken when a receipt pays the item.")
	}
	return nil
}

// discountNotGranted is the refusal of item i's cash discount.
func discountNotGranted(i int, detail string) error {
	return problem.Invalid("receivables.cash_discount_not_granted", fmt.Sprintf("items[%d].cash_discount", i), detail)
}
