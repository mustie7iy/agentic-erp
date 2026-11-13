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
// an amount against its limit (ADR-0066 §3-§4, T77a, T77b). Every amount
// is minor units of the company's currency, whose minor-unit digits are
// Exponent. Owed is what the customer's Items open items add up to as
// booked, credits netted; Ordered is what its Orders confirmed sales
// orders not yet billed come to, net plus output tax; Total is the two
// together, the exposure. Limit is the customer's limit, nil when none
// is set; Available is the limit less Total, set with it. Checked is the
// amount asked about, nil when none was given. Within says whether Total
// and Checked together are no more than the limit: always true without a
// limit, which restricts nothing.
type Exposure struct {
	Company   tenancy.Entity
	Exponent  int
	Customer  partner.Named
	Items     int32
	Owed      money.Minor
	Orders    int32
	Ordered   money.Minor
	Total     money.Minor
	Limit     *Limit
	Available money.Minor
	Checked   *money.Minor
	Within    bool
}

// ExposureOf computes a customer's exposure in a company the caller may
// act in, from the ledger's open customer items and the sales context's
// confirmed orders not yet billed as they are now, and checks the
// amount, when one is given, against the customer's limit. It
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
	if err := e.measure(ctx, &x, le.ID, customer); err != nil {
		return Exposure{}, err
	}
	l, ok, err := limitOf(ctx, tx, le.ID, customer, false)
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

// Check answers the sales context when it confirms an order (ADR-0066,
// As built T77b): whether the amount, the order's gross value in the
// company's currency, and the customer's exposure together are within
// its limit in the company. The customer's key in the company is held
// until the request's transaction ends (hold; ADR-0008), whether it has
// a limit or not, and the limit and the exposure are read after it is
// held, so of two orders of one customer confirmed at once the second is
// checked against an exposure that holds the first: they cannot both
// take the room only one fits; and a limit is not set beside a
// confirmation under way, which the next check would not see. The
// limit's row is read FOR UPDATE as well, so a change of the limit waits
// too. A customer without a limit is within it, and its exposure is not
// read: no limit restricts nothing. The caller has checked the caller's access to the
// order's company; Company, Customer and Exponent are not set.
func (e *Endpoints) Check(ctx context.Context, legalEntity, customer uuid.UUID, amount money.Minor) (Exposure, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Exposure{}, err
	}
	if err := hold(ctx, tx, legalEntity, customer); err != nil {
		return Exposure{}, err
	}
	l, ok, err := limitOf(ctx, tx, legalEntity, customer, true)
	if err != nil {
		return Exposure{}, err
	}
	x := Exposure{Checked: &amount}
	if !ok {
		x.Within = true
		return x, nil
	}
	x.Limit = &l
	if err := e.measure(ctx, &x, legalEntity, customer); err != nil {
		return Exposure{}, err
	}
	return x, x.check()
}

// measure gives the exposure its two parts and their sum: what the
// customer's confirmed orders not yet billed come to, and what it owes,
// the sum of its open items in the company's currency as booked, credits
// netted. The orders are read first: an invoice posted for an order
// between the two reads is then counted in both, never in neither (the
// read of GET /v1/credit-exposure is a snapshot, and sees one state).
func (e *Endpoints) measure(ctx context.Context, x *Exposure, legalEntity, customer uuid.UUID) error {
	var err error
	if x.Orders, x.Ordered, err = e.Orders.OpenOrders(ctx, legalEntity, customer); err != nil {
		return fmt.Errorf("credit: open orders of customer %s in legal entity %s: %w", customer, legalEntity, err)
	}
	open, err := e.OpenItems.Receivable(ctx, legalEntity, []uuid.UUID{customer})
	if err != nil {
		return fmt.Errorf("credit: open items of customer %s in legal entity %s: %w", customer, legalEntity, err)
	}
	booked := make([]money.Minor, len(open))
	for i, it := range open {
		booked[i] = it.AmountLC
	}
	if x.Owed, err = money.Sum(booked...); err != nil { // money.Sum's only error is the range
		return errTooLarge()
	}
	x.Items = int32(len(open))
	if x.Total, err = money.Sum(x.Owed, x.Ordered); err != nil {
		return errTooLarge()
	}
	return nil
}

// check compares the exposure, and the amount asked about, with the
// limit: what is available is the limit less the exposure, and the
// amount is within the limit when both together are no more than it.
// Amounts out of range are refused rather than compared wrongly.
func (x *Exposure) check() error {
	var err error
	if x.Available, err = money.Diff(x.Limit.Amount, x.Total); err != nil { // money.Diff's only error is the range
		return errTooLarge()
	}
	total := x.Total
	if x.Checked != nil {
		if total, err = money.Sum(x.Total, *x.Checked); err != nil {
			return errTooLarge()
		}
	}
	x.Within = total <= x.Limit.Amount
	return nil
}
