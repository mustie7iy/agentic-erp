package sales

import (
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// priced is what pricing gives an item: its unit price per price unit and
// its net value, in minor units of the document's currency.
type priced struct {
	UnitPrice money.Minor
	PriceUnit int32
	Net       money.Minor
}

// price prices item i of a quotation or a sales order, whose quantity
// items has checked. It is the one place an item is priced, for both
// documents (ADR-0067 §3): the price is the one entered with the item, in
// the document's currency per its price unit, and the item's net value is
// quantity × unit price ÷ price unit, rounded half away from zero to the
// currency's minor unit (money.Scale) and more than zero. Price
// determination (T80a) changes this function alone.
func price(doc document, i int, it ItemInput) (priced, error) {
	field := fmt.Sprintf("items[%d].unit_price", i)
	if it.UnitPrice.Currency != doc.currency {
		return priced{}, problem.Invalid("sales.currency_mismatch", field+".currency", "Every amount of a sales document is in its currency, "+doc.currency+".")
	}
	unit, err := currency.ParseAmount(it.UnitPrice.Value, doc.currency, doc.exp, field+".amount")
	if err != nil {
		return priced{}, fmt.Errorf("sales: unit price of item %d: %w", i+1, err)
	}
	net, err := money.Scale(unit, it.Quantity, int64(it.PriceUnit))
	if err != nil { // the quantity is a plain decimal and the price unit positive: what fails is the range
		return priced{}, errTooLarge()
	}
	if net <= 0 {
		return priced{}, problem.Invalid("sales.amount_not_positive", field+".amount",
			"An item's net value, quantity × unit price ÷ price unit, is more than zero.")
	}
	return priced{UnitPrice: unit, PriceUnit: it.PriceUnit, Net: net}, nil
}
