package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
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

// Condition is a condition the pricing procedure applied to an item
// (ADR-0067 §11): the price, with its unit price per price unit, or a
// discount, with its percentage of the value before it. Amount is what it
// adds to the item's net value in minor units of the document's currency,
// a discount's negative; Name is the condition type's when the item was
// priced.
type Condition struct {
	Type      string       `json:"condition_type"`
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
	UnitPrice *money.Minor `json:"unit_price,omitempty"`
	PriceUnit *int32       `json:"price_unit,omitempty"`
	Percent   *string      `json:"percent,omitempty"`
	Amount    money.Minor  `json:"amount"`
}

// priced is what pricing gives an item: its unit price per price unit and
// its net value, in minor units of the document's currency, where the
// price came from and the conditions applied, none without a procedure.
type priced struct {
	UnitPrice  money.Minor
	PriceUnit  int32
	Net        money.Minor
	Source     string
	Conditions []Condition
}

// price prices item i of a quotation or a sales order, whose quantity and
// item key items has checked. It is the one place an item is priced, for
// both documents (ADR-0067 §3, §10, §11). A price entered with the item
// stays, in the document's currency per its price unit, whatever the
// price records say; an item without one takes the price record valid for
// its item key on the document's date (determined). The item's value is
// quantity × unit price ÷ price unit, rounded half away from zero to the
// currency's minor unit (money.Scale) and more than zero. A tenant with a
// pricing procedure then has its discounts taken off a determined price
// (conditioned), never off an entered one; the net value stays more than
// zero. The price is fixed here, when the item
// is created: a later record does not change it.
func price(ctx context.Context, tx pgx.Tx, doc document, i int, it ItemInput) (priced, error) {
	p, field, err := unitPrice(ctx, tx, doc, i, it)
	if err != nil {
		return priced{}, err
	}
	p.Net, err = money.Scale(p.UnitPrice, it.Quantity, int64(p.PriceUnit))
	if err != nil { // the quantity is a plain decimal and the price unit positive: what fails is the range
		return priced{}, errTooLarge()
	}
	if p.Net <= 0 {
		return priced{}, problem.Invalid("sales.amount_not_positive", field,
			"An item's net value, quantity × unit price ÷ price unit, is more than zero.")
	}
	if len(doc.procedure) == 0 {
		return p, nil
	}
	if p, err = conditioned(ctx, tx, doc, it, p); err != nil {
		return priced{}, err
	}
	if p.Net <= 0 {
		return priced{}, problem.Invalid("sales.amount_not_positive", field, "An item's net value, its value less its discounts, is more than zero.")
	}
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

// validRecord is the SQL that picks, among an item key's records of the
// document's company that are valid on its date, the one that applies
// (ADR-0067 §10): the customer's before the general one; among several
// the latest valid_from, and of two with the same valid_from the one
// recorded last, so the choice never depends on the order rows are read
// in. $1 is the company, $2 the item key, $3 the customer and $4 the
// date; which is what else the record must be, on parameters from $5.
func validRecord(columns, which string) string {
	return `SELECT ` + columns + ` FROM sales.price
		WHERE legal_entity_id = $1 AND item_key = $2 AND (customer_id IS NULL OR customer_id = $3)
			AND valid_from <= $4 AND (valid_to IS NULL OR valid_to >= $4) AND ` + which + `
		ORDER BY customer_id IS NULL, valid_from DESC, created_at DESC, id DESC LIMIT 1`
}

// determined is the price an item without one takes (ADR-0067 §10): the
// record of its item key that applies (validRecord) among the prices in
// the item's unit of measure and the document's currency. A price is a
// record without a condition type or, for a tenant with a pricing
// procedure, of the procedure's price type; a record of another type
// waits for a procedure that names it. An item without a key, or without
// such a record, is refused naming the price it lacks (422
// sales.no_price).
func determined(ctx context.Context, tx pgx.Tx, doc document, field string, it ItemInput) (priced, error) {
	if it.ItemKey == nil {
		return priced{}, problem.Invalid("sales.no_price", field, "An item has a unit price, or an item key that a price is recorded for.")
	}
	priceType := "" // no condition type has this code
	if len(doc.procedure) > 0 {
		priceType = doc.procedure[0].Code
	}
	p := priced{Source: PriceList}
	err := tx.QueryRow(ctx, validRecord(`unit_price, price_unit`,
		`percent IS NULL AND uom_code = $5 AND currency = $6 AND (condition_type IS NULL OR condition_type = $7)`),
		doc.le.ID, *it.ItemKey, doc.customer, doc.date, it.Unit, doc.currency, priceType).Scan(&p.UnitPrice, &p.PriceUnit)
	if errors.Is(err, pgx.ErrNoRows) {
		return priced{}, problem.Invalid("sales.no_price", field, "No price is recorded for item key "+*it.ItemKey+" in "+it.Unit+" and "+doc.currency+
			" that is valid on "+doc.date.Format(time.DateOnly)+", for the customer or for every customer; give the item a unit price or record one.")
	}
	if err != nil {
		return priced{}, fmt.Errorf("sales: find the price of item key %s: %w", *it.ItemKey, err)
	}
	return p, nil
}

// conditioned applies the tenant's pricing procedure to an item whose
// price and value p holds (ADR-0067 §11). The procedure's first step is
// the price: the one entered or determined. Each later step is a
// discount type: the record of that type that applies to the item's key
// (validRecord), if there is one, takes its percentage off the value
// before it, rounded half away from zero to the currency's minor unit
// (money.Scale). The conditions are listed in the procedure's order and
// their amounts add up to the net value. A price entered with the item
// is its final net price (PO 2026-11-12, T80c): no discount is taken off
// it, and its price is its only condition. An item without a key has no
// discount either: discounts are recorded by item key.
func conditioned(ctx context.Context, tx pgx.Tx, doc document, it ItemInput, p priced) (priced, error) {
	first, unit, per := doc.procedure[0], p.UnitPrice, p.PriceUnit
	p.Conditions = []Condition{{Type: first.Code, Name: first.Name, Kind: first.Kind, UnitPrice: &unit, PriceUnit: &per, Amount: p.Net}}
	if it.ItemKey == nil || p.Source == Manual {
		return p, nil
	}
	for _, step := range doc.procedure[1:] {
		var percent string
		var off money.Minor
		err := tx.QueryRow(ctx, validRecord(`trim_scale(percent)::text`, `percent IS NOT NULL AND condition_type = $5`),
			doc.le.ID, *it.ItemKey, doc.customer, doc.date, step.Code).Scan(&percent)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err == nil {
			// The percentage is the record's, a decimal below 100, so the
			// discount is within the value's range.
			off, err = money.Scale(p.Net, percent, 100)
		}
		if err != nil {
			return priced{}, fmt.Errorf("sales: discount %s of item key %s: %w", step.Code, *it.ItemKey, err)
		}
		p.Net -= off
		p.Conditions = append(p.Conditions, Condition{Type: step.Code, Name: step.Name, Kind: step.Kind, Percent: &percent, Amount: -off})
	}
	return p, nil
}

// procedure is the tenant's pricing procedure from the active
// configuration: its condition types in the order they apply, none for a
// tenant without one (ADR-0067 §11).
func (e *Endpoints) procedure(ctx context.Context) ([]config.ConditionType, error) {
	cfg, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("sales: pricing procedure: %w", err)
	}
	return cfg.Pricing.Steps(), nil
}
