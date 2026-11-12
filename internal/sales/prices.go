package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Price is a row of sales.price (ADR-0067 §10): the net price of an item
// key in a company, for every customer (CustomerID nil) or for one, per
// PriceUnit of Unit, in minor units of Currency, whose minor-unit digits
// are Exponent, valid from a day and, when ValidTo is set, until it. A
// record is written once and never changed.
type Price struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	ItemKey       string
	CustomerID    *uuid.UUID
	Unit          string
	Currency      string
	Exponent      int
	UnitPrice     money.Minor
	PriceUnit     int32
	ValidFrom     time.Time
	ValidTo       *time.Time
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
}

// PriceInput is a price record as a client sends it.
type PriceInput struct {
	LegalEntityID uuid.UUID
	ItemKey       string
	CustomerID    *uuid.UUID
	Unit          string
	UnitPrice     Amount
	PriceUnit     int32
	ValidFrom     time.Time
	ValidTo       *time.Time
}

const priceSelect = `SELECT p.id, p.legal_entity_id, p.item_key, p.customer_id, p.uom_code, p.currency, c.exponent, p.unit_price, p.price_unit,
		p.valid_from, p.valid_to, p.created_at, p.created_by
	FROM sales.price p JOIN ref.currency c ON c.code = p.currency`

func scanPrice(row pgx.Row) (Price, error) {
	var p Price
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.ItemKey, &p.CustomerID, &p.Unit, &p.Currency, &p.Exponent, &p.UnitPrice, &p.PriceUnit,
		&p.ValidFrom, &p.ValidTo, &p.CreatedAt, &p.CreatedBy)
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

// RecordPrice records a price for an item key in a company (ADR-0067
// §10), for every customer or for one that may be sold to there, per a
// price unit of a unit of measure of the reference data, more than zero
// in a known currency, valid from a day until one not before it. Records
// are insert-only: a new price is a new record, which determination
// prefers by its later valid_from (pricing.go). It publishes
// SalesPriceChanged with its audit record.
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
	amount, err := priceAmount(ctx, tx, in)
	if err != nil {
		return Price{}, err
	}
	if in.ValidTo != nil && in.ValidTo.Before(in.ValidFrom) {
		return Price{}, problem.Invalid("sales.invalid_validity", "valid_to",
			"A price is valid until a day not before the day it is valid from, "+in.ValidFrom.Format(time.DateOnly)+".")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, customer_id, uom_code, currency, unit_price,
			price_unit, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		p.TenantID, id, le.ID, in.ItemKey, in.CustomerID, in.Unit, in.UnitPrice.Currency, amount, in.PriceUnit, in.ValidFrom, in.ValidTo, p.ID); err != nil {
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

// priceAmount is the record's unit price in minor units of its currency:
// more than zero (422 sales.amount_not_positive naming
// unit_price.amount), in a known currency (422 sales.unknown_currency
// naming unit_price.currency), per a unit of measure of the reference
// data (422 sales.unknown_unit naming unit).
func priceAmount(ctx context.Context, tx pgx.Tx, in PriceInput) (money.Minor, error) {
	units, err := knownUnits(ctx, tx, []string{in.Unit})
	if err != nil {
		return 0, err
	}
	if !units[in.Unit] {
		return 0, problem.Invalid("sales.unknown_unit", "unit", "Unit of measure "+in.Unit+" is not known.")
	}
	exp, err := currency.Exponent(ctx, tx, in.UnitPrice.Currency, problem.Invalid("sales.unknown_currency", "unit_price.currency", "The currency is not known."))
	if err != nil {
		return 0, fmt.Errorf("sales: currency %s of a price: %w", in.UnitPrice.Currency, err)
	}
	amount, err := currency.ParseAmount(in.UnitPrice.Value, in.UnitPrice.Currency, exp, "unit_price.amount")
	if err != nil {
		return 0, fmt.Errorf("sales: unit price of a price record: %w", err)
	}
	if amount <= 0 {
		return 0, problem.Invalid("sales.amount_not_positive", "unit_price.amount", "A price is more than zero.")
	}
	return amount, nil
}

// recordPrice adds the audit record of a new price record and publishes
// SalesPriceChanged in the same transaction (ADR-0007, ADR-0011). The
// event names the customer by id and number, never by name (product
// owner, 2026-11-05); both are null for a general price.
func recordPrice(ctx context.Context, p Price, le tenancy.Entity, customerNumber *string) error {
	amount := money.EventAmount(p.UnitPrice, p.Currency, p.Exponent)
	var validTo *string
	if p.ValidTo != nil {
		day := p.ValidTo.Format(time.DateOnly)
		validTo = &day
	}
	payload := map[string]any{"price_id": p.ID, "legal_entity_id": p.LegalEntityID, "legal_entity_code": le.Code, "item_key": p.ItemKey,
		"customer_id": p.CustomerID, "customer_number": customerNumber, "unit": p.Unit, "unit_price": amount, "price_unit": p.PriceUnit,
		"valid_from": p.ValidFrom.Format(time.DateOnly), "valid_to": validTo}
	detail := map[string]any{"legal_entity_id": p.LegalEntityID, "item_key": p.ItemKey, "customer_id": p.CustomerID, "unit": p.Unit,
		"unit_price": amount, "price_unit": p.PriceUnit, "valid_from": p.ValidFrom.Format(time.DateOnly), "valid_to": validTo}
	return record(ctx, "sales.price.create", "price", "sales_price_changed", p.ID, detail, payload)
}
