package sales

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Price is a row of sales.price (ADR-0067 §10, §11): a record of an item
// key in a company, for every customer (CustomerID nil) or for one, valid
// from a day and, when ValidTo is set, until it. It is a price, the net
// price per PriceUnit of Unit in minor units of Currency, whose
// minor-unit digits are Exponent, or a discount of a condition type, a
// Percent of an item's value and nothing of a price's. ConditionType is
// the record's condition type of the configuration, nil for a price
// recorded without one. A record is written once and never changed.
type Price struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	ItemKey       string
	CustomerID    *uuid.UUID
	ConditionType *string
	Unit          *string
	Currency      *string
	Exponent      *int
	UnitPrice     *money.Minor
	PriceUnit     *int32
	Percent       *string
	ValidFrom     time.Time
	ValidTo       *time.Time
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
}

// Kind is what the record is: a discount when it has a percent, else a
// price.
func (p Price) Kind() string {
	if p.Percent != nil {
		return config.ConditionDiscountPercent
	}
	return config.ConditionPrice
}

// PriceInput is a price record as a client sends it: a price gives Unit
// and UnitPrice, with PriceUnit when it is not 1; a discount gives
// ConditionType and Percent.
type PriceInput struct {
	LegalEntityID uuid.UUID
	ItemKey       string
	CustomerID    *uuid.UUID
	ConditionType *string
	Unit          *string
	UnitPrice     *Amount
	PriceUnit     *int32
	Percent       *string
	ValidFrom     time.Time
	ValidTo       *time.Time
}

const priceSelect = `SELECT p.id, p.legal_entity_id, p.item_key, p.customer_id, p.condition_type, p.uom_code, p.currency, c.exponent, p.unit_price,
		p.price_unit, trim_scale(p.percent)::text, p.valid_from, p.valid_to, p.created_at, p.created_by
	FROM sales.price p LEFT JOIN ref.currency c ON c.code = p.currency`

func scanPrice(row pgx.Row) (Price, error) {
	var p Price
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.ItemKey, &p.CustomerID, &p.ConditionType, &p.Unit, &p.Currency, &p.Exponent, &p.UnitPrice,
		&p.PriceUnit, &p.Percent, &p.ValidFrom, &p.ValidTo, &p.CreatedAt, &p.CreatedBy)
	return p, errs.Wrapf(err, "scan sales price")
}

// loadPrice reads a price record whatever its company; GetPrice applies
// company access.
func loadPrice(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Price, error) {
	p, err := scanPrice(tx.QueryRow(ctx, priceSelect+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Price{}, problem.NotFoundOf("sales price")
	}
	if err != nil {
		return Price{}, fmt.Errorf("sales: read price %s: %w", id, err)
	}
	return p, nil
}

// GetPrice returns a price record of a legal entity the caller may act
// in; another's is not found, so ids cannot be probed (04 §4.9).
func GetPrice(ctx context.Context, id uuid.UUID) (Price, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Price{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Price{}, fmt.Errorf("sales: company access for price %s: %w", id, err)
	}
	p, err := loadPrice(ctx, tx, id)
	if err == nil && !access.Allows(p.LegalEntityID) {
		return Price{}, problem.NotFoundOf("sales price")
	}
	return p, err
}

// rate is what a record says beyond its key and validity, checked: a
// price's unit, currency, amount and price unit, or a discount's percent.
type rate struct {
	unit, currency *string
	amount         *money.Minor
	priceUnit      *int32
	percent        *string
}

// RecordPrice records a price for an item key in a company (ADR-0067
// §10), or a discount on it (§11), for every customer or for one that may
// be sold to there, valid from a day until one not before it. A price is
// per a price unit of a unit of measure of the reference data, more than
// zero in a known currency; a discount is of a configured condition type
// of kind discount_percent, a percentage more than 0 and less than 100.
// Records are insert-only: a new price is a new record, which
// determination prefers by its later valid_from (pricing.go). It
// publishes SalesPriceChanged with its audit record.
func (e *Endpoints) RecordPrice(ctx context.Context, in PriceInput) (Price, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Price{}, err
	}
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return Price{}, err
	}
	if err := problem.RequireText("item_key", in.ItemKey); err != nil {
		return Price{}, fmt.Errorf("sales: item key of a price: %w", err)
	}
	customerNumber, err := e.priceCustomer(ctx, le, in.CustomerID)
	if err != nil {
		return Price{}, err
	}
	r, err := e.rate(ctx, tx, in)
	if err != nil {
		return Price{}, err
	}
	if in.ValidTo != nil && in.ValidTo.Before(in.ValidFrom) {
		return Price{}, problem.Invalid("sales.invalid_validity", "valid_to",
			"A price is valid until a day not before the day it is valid from, "+in.ValidFrom.Format(time.DateOnly)+".")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, customer_id, condition_type, uom_code, currency,
			unit_price, price_unit, percent, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12, $13, $14)`,
		p.TenantID, id, le.ID, in.ItemKey, in.CustomerID, in.ConditionType, r.unit, r.currency, r.amount, r.priceUnit, r.percent,
		in.ValidFrom, in.ValidTo, p.ID); err != nil {
		return Price{}, fmt.Errorf("sales: insert price %s: %w", id, err)
	}
	recorded, err := loadPrice(ctx, tx, id)
	if err != nil {
		return Price{}, err
	}
	return recorded, recordPrice(ctx, recorded, le, customerNumber)
}

// priceCustomer is the number of the customer a price is for, nil for a
// general price: a customer is checked as a quotation's is (customer).
func (e *Endpoints) priceCustomer(ctx context.Context, le tenancy.Entity, customer *uuid.UUID) (*string, error) {
	if customer == nil {
		return nil, nil
	}
	named, err := e.customer(ctx, le, *customer)
	if err != nil {
		return nil, err
	}
	return &named.Number, nil
}

// rate checks what the record says by its kind (ADR-0067 §11): a record
// without a condition type is a price; one with a type is what the type's
// kind in the active configuration says, and a type the configuration
// does not have is refused (422 sales.unknown_condition_type naming
// condition_type). A price gives a unit and a price and no percent, a
// discount a percent and nothing of a price (422 sales.price_or_percent
// naming the field given or missing).
func (e *Endpoints) rate(ctx context.Context, tx pgx.Tx, in PriceInput) (rate, error) {
	kind := config.ConditionPrice
	if in.ConditionType != nil {
		cfg, _, err := e.Settings.Document(ctx)
		if err != nil {
			return rate{}, fmt.Errorf("sales: condition type %s of a price: %w", *in.ConditionType, err)
		}
		t, ok := cfg.Pricing.ConditionType(*in.ConditionType)
		if !ok {
			return rate{}, problem.Invalid("sales.unknown_condition_type", "condition_type",
				"Condition type "+*in.ConditionType+" is not one of the configuration's (pricing.condition_types).")
		}
		kind = t.Kind
	}
	given := []struct {
		field    string
		set      bool
		discount bool // a discount's field, else a price's
		required bool
	}{{"unit", in.Unit != nil, false, true}, {"unit_price", in.UnitPrice != nil, false, true}, {"price_unit", in.PriceUnit != nil, false, false},
		{"percent", in.Percent != nil, true, true}}
	for _, g := range given {
		mine := g.discount == (kind == config.ConditionDiscountPercent)
		switch {
		case g.set && !mine:
			return rate{}, problem.Invalid("sales.price_or_percent", g.field,
				"A price gives unit and unit_price, with price_unit when it is not 1; a discount gives percent. This record is a "+kindName(kind)+".")
		case !g.set && mine && g.required:
			return rate{}, problem.Invalid("sales.price_or_percent", g.field, "A "+kindName(kind)+" gives "+g.field+".")
		}
	}
	if kind == config.ConditionDiscountPercent {
		return discountRate(*in.Percent)
	}
	return priceRate(ctx, tx, in)
}

// kindName names a condition kind in a message.
func kindName(kind string) string {
	if kind == config.ConditionDiscountPercent {
		return "discount"
	}
	return "price"
}

// hundred is the percentage a discount stays below.
var hundred = big.NewRat(100, 1)

// discountRate is a discount's percent: a plain decimal more than 0 and
// less than 100 of at most six decimals (422 sales.invalid_percent naming
// percent). A discount of 100 percent would leave an item no value, which
// no item may have (ADR-0067 default 4).
func discountRate(percent string) (rate, error) {
	if money.PositiveQuantity(percent) != nil || money.ParseRat(percent).Cmp(hundred) >= 0 {
		return rate{}, problem.Invalid("sales.invalid_percent", "percent",
			fmt.Sprintf("A discount is a percentage more than 0 and less than 100, a plain decimal of at most %d decimals.", money.QuantityDecimals))
	}
	return rate{percent: &percent}, nil
}

// priceRate is a price's unit price in minor units of its currency: more
// than zero (422 sales.amount_not_positive naming unit_price.amount), in
// a known currency (422 sales.unknown_currency naming
// unit_price.currency), per a price unit, 1 when none is given, of a unit
// of measure of the reference data (422 sales.unknown_unit naming unit).
func priceRate(ctx context.Context, tx pgx.Tx, in PriceInput) (rate, error) {
	units, err := knownUnits(ctx, tx, []string{*in.Unit})
	if err != nil {
		return rate{}, err
	}
	if !units[*in.Unit] {
		return rate{}, problem.Invalid("sales.unknown_unit", "unit", "Unit of measure "+*in.Unit+" is not known.")
	}
	code := in.UnitPrice.Currency
	exp, err := currency.Exponent(ctx, tx, code, problem.Invalid("sales.unknown_currency", "unit_price.currency", "The currency is not known."))
	if err != nil {
		return rate{}, fmt.Errorf("sales: currency %s of a price: %w", code, err)
	}
	amount, err := currency.ParseAmount(in.UnitPrice.Value, code, exp, "unit_price.amount")
	if err != nil {
		return rate{}, fmt.Errorf("sales: unit price of a price record: %w", err)
	}
	if amount <= 0 {
		return rate{}, problem.Invalid("sales.amount_not_positive", "unit_price.amount", "A price is more than zero.")
	}
	priceUnit := int32(1)
	if in.PriceUnit != nil {
		priceUnit = *in.PriceUnit
	}
	return rate{unit: in.Unit, currency: &code, amount: &amount, priceUnit: &priceUnit}, nil
}

// recordPrice adds the audit record of a new price record and publishes
// SalesPriceChanged in the same transaction (ADR-0007, ADR-0011). The
// event names the customer by id and number, never by name (product
// owner, 2026-11-05); both are null for a general record. A price's
// unit, unit price and price unit are null for a discount, and a
// discount's percent for a price.
func recordPrice(ctx context.Context, p Price, le tenancy.Entity, customerNumber *string) error {
	var amount map[string]string
	if p.UnitPrice != nil {
		amount = money.EventAmount(*p.UnitPrice, *p.Currency, *p.Exponent)
	}
	var validTo *string
	if p.ValidTo != nil {
		day := p.ValidTo.Format(time.DateOnly)
		validTo = &day
	}
	payload := map[string]any{"price_id": p.ID, "legal_entity_id": p.LegalEntityID, "legal_entity_code": le.Code, "item_key": p.ItemKey,
		"customer_id": p.CustomerID, "customer_number": customerNumber, "kind": p.Kind(), "condition_type": p.ConditionType, "unit": p.Unit,
		"unit_price": amount, "price_unit": p.PriceUnit, "percent": p.Percent, "valid_from": p.ValidFrom.Format(time.DateOnly), "valid_to": validTo}
	detail := map[string]any{"legal_entity_id": p.LegalEntityID, "item_key": p.ItemKey, "customer_id": p.CustomerID, "kind": p.Kind(),
		"condition_type": p.ConditionType, "unit": p.Unit, "unit_price": amount, "price_unit": p.PriceUnit, "percent": p.Percent,
		"valid_from": p.ValidFrom.Format(time.DateOnly), "valid_to": validTo}
	return record(ctx, "sales.price.create", "price", "sales_price_changed", p.ID, detail, payload)
}
