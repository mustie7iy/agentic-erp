package ledger

import (
	"context"
	"errors"
	"fmt"
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
	From, To       string
	Rate           string // units of To per unit of From; "1" for one currency
	FromExp, ToExp int
}

// Convert translates an amount (money.Convert): rounded half away from
// zero, a result out of range is money.ErrOverflow.
func (c Conversion) Convert(amount money.Minor) (money.Minor, error) {
	v, err := money.Convert(amount, c.Rate, c.FromExp, c.ToExp)
	if err != nil {
		return 0, fmt.Errorf("ledger: convert %s into %s: %w", c.From, c.To, err)
	}
	return v, nil
}

// errConversionTooLarge refuses a document amount whose value in the
// company's currency is out of range.
func errConversionTooLarge() error {
	return problem.Unprocessable("ledger.amount_too_large",
		"The amount in the company's currency is too large to compare with its configured amounts.")
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
	c := Conversion{From: from, To: to, Rate: r}
	if c.FromExp, err = currency.Exponent(ctx, tx, from, nil); err != nil {
		return Conversion{}, fmt.Errorf("ledger: conversion %s/%s: %w", from, to, err)
	}
	if c.ToExp, err = currency.Exponent(ctx, tx, to, nil); err != nil {
		return Conversion{}, fmt.Errorf("ledger: conversion %s/%s: %w", from, to, err)
	}
	return c, nil
}

// ToCompany converts an amount in the currency into the legal entity's
// local currency at the rate valid on the date, and returns it with that
// currency's minor-unit digits.
func (r Rates) ToCompany(ctx context.Context, legalEntity uuid.UUID, currency string, date time.Time, amount money.Minor) (money.Minor, int, error) {
	le, ok, err := r.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, fmt.Errorf("ledger: convert into the currency of legal entity %s: no such legal entity", legalEntity)
	}
	c, err := r.Conversion(ctx, currency, le.LocalCurrency, date)
	if err != nil {
		return 0, 0, err
	}
	v, err := c.Convert(amount)
	if errors.Is(err, money.ErrOverflow) {
		return 0, 0, errConversionTooLarge()
	}
	return v, c.ToExp, err
}
