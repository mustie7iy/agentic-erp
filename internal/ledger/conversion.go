package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Rates converts other contexts' document amounts into a company's
// currency, the legal entity's local currency, at the rate a posting of the
// document would use: the average rate valid on the date (rate). The
// configuration's amounts (requisition approval thresholds, invoice price
// tolerances, the auto-post limit) are in the company's currency, so a
// document in another currency is converted before it is compared with
// them (F12); a missing rate refuses the comparison rather than comparing
// numbers in two currencies.
type Rates struct {
	LegalEntities LegalEntities
}

// Conversion translates minor units of one currency into another at a
// rate valid on a date.
type Conversion struct {
	From, To string
	// Rate is units of To per unit of From, "1" for one currency, to 12
	// decimals: for people. When only the reverse pair is recorded it is
	// the reciprocal rounded, and amounts are converted at the exact
	// reciprocal instead (F19). A Conversion made with Rate alone converts
	// at Rate.
	Rate           string
	FromExp, ToExp int
	exact          *big.Rat
}

// Convert translates an amount (money.Convert): rounded once, half away
// from zero; a result out of range is money.ErrOverflow.
func (c Conversion) Convert(amount money.Minor) (money.Minor, error) {
	v, err := c.Exact(amount)
	if err != nil {
		return 0, err
	}
	m, err := money.Round(v, c.ToExp)
	if err != nil {
		return 0, c.failed(err)
	}
	return m, nil
}

// Exact is an amount's value in major units of the target currency,
// unrounded (money.Exact): what is compared with the company's configured
// amounts, while Convert's rounded value is what people are shown and what
// is posted (F13).
func (c Conversion) Exact(amount money.Minor) (*big.Rat, error) {
	r := c.exact
	if r == nil {
		var err error
		if r, err = money.ParsePlain(c.Rate); err != nil {
			return nil, c.failed(err)
		}
	}
	v, err := money.ExactAt(amount, r, c.FromExp)
	if err != nil {
		return nil, c.failed(err)
	}
	return v, nil
}

// failed names the pair of a failed conversion.
func (c Conversion) failed(err error) error {
	return fmt.Errorf("ledger: convert %s into %s: %w", c.From, c.To, err)
}

// Conversion returns the conversion from one currency into another on the
// date. Without a rate it refuses with ledger.no_exchange_rate, naming the
// pair and the date.
func (Rates) Conversion(ctx context.Context, from, to string, date time.Time) (Conversion, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Conversion{}, err
	}
	r, err := rate(ctx, tx, from, to, date)
	var p *problem.Problem
	if errors.As(err, &p) && p.Code == "ledger.no_exchange_rate" {
		// rate names the posting date, a field of journal entries; here the
		// date is the document's and the rate is what is missing.
		return Conversion{}, problem.Unprocessable("ledger.no_exchange_rate",
			fmt.Sprintf("No average exchange rate from %s to %s is valid on %s; the document is compared with the company's configured amounts in %s.",
				from, to, date.Format(time.DateOnly), to),
			problem.FieldError{Field: "currency", Code: "no_exchange_rate", Message: "record the average rate into the company's currency first"})
	}
	if err != nil {
		return Conversion{}, err
	}
	c := Conversion{From: from, To: to, Rate: r.shown, exact: r.exact}
	if c.FromExp, err = currency.Exponent(ctx, tx, from, nil); err != nil {
		return Conversion{}, fmt.Errorf("ledger: conversion %s/%s: %w", from, to, err)
	}
	if c.ToExp, err = currency.Exponent(ctx, tx, to, nil); err != nil {
		return Conversion{}, fmt.Errorf("ledger: conversion %s/%s: %w", from, to, err)
	}
	return c, nil
}

// ExactToCompany converts an amount in the currency from into the legal
// entity's local currency at the rate valid on the date, unrounded
// (Conversion.Exact): its value in major units of that currency, for
// comparing with the company's configured amounts, where rounding to the
// currency's digits first would let 1000.004 pass a limit of 1000.00
// (F13). Callers pass the legal entity of a stored document, which always
// exists; an unknown one is still refused as the reference it is rather
// than failing the request.
func (r Rates) ExactToCompany(ctx context.Context, legalEntity uuid.UUID, from string, date time.Time, amount money.Minor) (*big.Rat, error) {
	le, ok, err := r.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("ledger: convert into the currency of legal entity %s: %w", legalEntity, err)
	}
	if !ok {
		return nil, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id")
	}
	c, err := r.Conversion(ctx, from, le.LocalCurrency, date)
	if err != nil {
		return nil, err
	}
	return c.Exact(amount)
}
