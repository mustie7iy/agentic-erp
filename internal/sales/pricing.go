package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Where an item's unit price came from (ADR-0067 §10): entered with the
// item, or the price record valid on the document's date.
const (
	Manual    = "manual"
	PriceList = "price_list"
)

// priced is what pricing gives an item: its unit price per price unit and
// its net value, in minor units of the document's currency, and where the
// price came from.
type priced struct {
	UnitPrice money.Minor
	PriceUnit int32
	Net       money.Minor
	Source    string
}

// price prices item i of a quotation or a sales order, whose quantity and
// item key items has checked. It is the one place an item is priced, for
// both documents (ADR-0067 §3, §10). A price entered with the item stays,
// in the document's currency per its price unit, whatever the price
// records say; an item without one takes the price record valid for its
// item key on the document's date (determined). The item's net value is
// quantity × unit price ÷ price unit, rounded half away from zero to the
// currency's minor unit (money.Scale) and more than zero. The price is
// fixed here, when the item is created: a later record does not change
// it.
func price(ctx context.Context, tx pgx.Tx, doc document, i int, it ItemInput) (priced, error) {
	p, field, err := unitPrice(ctx, tx, doc, i, it)
	if err != nil {
		return priced{}, err
	}
	net, err := money.Scale(p.UnitPrice, it.Quantity, int64(p.PriceUnit))
	if err != nil { // the quantity is a plain decimal and the price unit positive: what fails is the range
		return priced{}, errTooLarge()
	}
	if net <= 0 {
		return priced{}, problem.Invalid("sales.amount_not_positive", field,
			"An item's net value, quantity × unit price ÷ price unit, is more than zero.")
	}
	p.Net = net
	return p, nil
}

// unitPrice is item i's unit price and its price unit: the ones entered
// with it, else the price record's. field is the request's field a net
// value of zero is refused on: the price entered, or the quantity of an
// item whose price was not the request's.
func unitPrice(ctx context.Context, tx pgx.Tx, doc document, i int, it ItemInput) (priced, string, error) {
	field := fmt.Sprintf("items[%d].unit_price", i)
	if it.UnitPrice == nil {
		p, err := determined(ctx, tx, doc, field, it)
		return p, fmt.Sprintf("items[%d].quantity.value", i), err
	}
	if it.UnitPrice.Currency != doc.currency {
		return priced{}, "", problem.Invalid("sales.currency_mismatch", field+".currency", "Every amount of a sales document is in its currency, "+doc.currency+".")
	}
	unit, err := currency.ParseAmount(it.UnitPrice.Value, doc.currency, doc.exp, field+".amount")
	if err != nil {
		return priced{}, "", fmt.Errorf("sales: unit price of item %d: %w", i+1, err)
	}
	return priced{UnitPrice: unit, PriceUnit: it.PriceUnit, Source: Manual}, field + ".amount", nil
}

// determined is the price an item without one takes (ADR-0067 §10): the
// record of its item key in the document's company that is valid on the
// document's date, in the item's unit of measure and the document's
// currency. The customer's price wins over the general one; among several
// the latest valid_from, and of two with the same valid_from the one
// recorded last, so the choice never depends on the order rows are read
// in. An item without a key, or without such a record, is refused naming
// the price it lacks (422 sales.no_price).
func determined(ctx context.Context, tx pgx.Tx, doc document, field string, it ItemInput) (priced, error) {
	if it.ItemKey == nil {
		return priced{}, problem.Invalid("sales.no_price", field, "An item has a unit price, or an item key that a price is recorded for.")
	}
	p := priced{Source: PriceList}
	err := tx.QueryRow(ctx, `SELECT unit_price, price_unit FROM sales.price
		WHERE legal_entity_id = $1 AND item_key = $2 AND uom_code = $3 AND currency = $4 AND (customer_id IS NULL OR customer_id = $5)
			AND valid_from <= $6 AND (valid_to IS NULL OR valid_to >= $6)
		ORDER BY customer_id IS NULL, valid_from DESC, created_at DESC, id DESC LIMIT 1`,
		doc.le.ID, *it.ItemKey, it.Unit, doc.currency, doc.customer, doc.date).Scan(&p.UnitPrice, &p.PriceUnit)
	if errors.Is(err, pgx.ErrNoRows) {
		return priced{}, problem.Invalid("sales.no_price", field, "No price is recorded for item key "+*it.ItemKey+" in "+it.Unit+" and "+doc.currency+
			" that is valid on "+doc.date.Format(time.DateOnly)+", for the customer or for every customer; give the item a unit price or record one.")
	}
	if err != nil {
		return priced{}, fmt.Errorf("sales: find the price of item key %s: %w", *it.ItemKey, err)
	}
	return p, nil
}
