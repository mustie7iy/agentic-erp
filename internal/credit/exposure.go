package credit

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Exposure is a customer's credit exposure in a company and the check of
// an amount against its limit (ADR-0066 §3-§4, T77a). Every amount is
// minor units of the company's currency, whose minor-unit digits are
// Exponent. Owed is what the customer's Items open items add up to as
// booked, credits netted. Limit is the customer's limit, nil when none is
// set; Available is the limit less Owed, set with it. Checked is the
// amount the request asked about, nil when it gave none. Within says
// whether Owed and Checked together are no more than the limit: always
// true without a limit, which restricts nothing.
type Exposure struct {
	Company   tenancy.Entity
	Exponent  int
	Customer  partner.Named
	Items     int32
	Owed      money.Minor
	Limit     *Limit
	Available money.Minor
	Checked   *money.Minor
	Within    bool
}

// ExposureOf computes a customer's exposure in a company the caller may
// act in, from the ledger's open customer items as they are now, and
// checks the amount, when one is given, against the customer's limit. It
// stores nothing, changes nothing and blocks nothing. Any business
// partner can be asked about: one posted to without customer data in the
// company has open items all the same (ADR-0063), and one without any has
// an exposure of zero.
func (e *Endpoints) ExposureOf(ctx context.Context, legalEntity, customer uuid.UUID, amount *string) (Exposure, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Exposure{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Exposure{}, fmt.Errorf("credit: company access for the credit exposure: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return Exposure{}, fmt.Errorf("credit: company %s of the credit exposure: %w", legalEntity, err)
	}
	if !ok || !access.Allows(legalEntity) {
		return Exposure{}, problem.NotFoundOf("legal entity") // another company's is not found, so ids cannot be probed
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{customer})
	if err != nil {
		return Exposure{}, fmt.Errorf("credit: customer %s of the credit exposure: %w", customer, err)
	}
	named, ok := names[customer]
	if !ok {
		return Exposure{}, problem.NotFoundOf("business partner")
	}
	x := Exposure{Company: le, Customer: named}
	if x.Exponent, err = currency.Exponent(ctx, tx, le.LocalCurrency, nil); err != nil { // a legal entity's currency exists
		return Exposure{}, fmt.Errorf("credit: currency of company %s: %w", le.Code, err)
	}
	if amount != nil {
		checked, err := amountOf(*amount, le.LocalCurrency, x.Exponent, "amount")
		if err != nil {
			return Exposure{}, err
		}
		x.Checked = &checked
	}
	if err := e.owed(ctx, &x, customer); err != nil {
		return Exposure{}, err
	}
	l, ok, err := limitOf(ctx, tx, le.ID, customer)
	if err != nil {
		return Exposure{}, err
	}
	if !ok {
		x.Within = true // no limit is set: the customer is not restricted
		return x, nil
	}
	x.Limit = &l
	return x, x.check()
}

// owed gives the exposure what the customer owes: the sum of its open
// items in the company's currency as booked, credits netted.
func (e *Endpoints) owed(ctx context.Context, x *Exposure, customer uuid.UUID) error {
	open, err := e.OpenItems.Receivable(ctx, x.Company.ID, []uuid.UUID{customer})
	if err != nil {
		return fmt.Errorf("credit: open items of customer %s in company %s: %w", x.Customer.Number, x.Company.Code, err)
	}
	booked := make([]money.Minor, len(open))
	for i, it := range open {
		booked[i] = it.AmountLC
	}
	if x.Owed, err = money.Sum(booked...); err != nil { // money.Sum's only error is the range
		return errTooLarge()
	}
	x.Items = int32(len(open))
	return nil
}

// check compares what the customer owes, and the amount asked about,
// with its limit: what is available is the limit less what it owes, and
// the amount is within the limit when both together are no more than it.
// Amounts out of range are refused rather than compared wrongly.
func (x *Exposure) check() error {
	var err error
	if x.Available, err = money.Diff(x.Limit.Amount, x.Owed); err != nil { // money.Diff's only error is the range
		return errTooLarge()
	}
	total := x.Owed
	if x.Checked != nil {
		if total, err = money.Sum(x.Owed, *x.Checked); err != nil {
			return errTooLarge()
		}
	}
	x.Within = total <= x.Limit.Amount
	return nil
}
