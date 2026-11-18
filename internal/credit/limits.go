package credit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LimitInput is a credit limit as a client sets it: the amount as the
// request gives it, a decimal string in Currency, and its validity and
// next review date, each nil when absent (T77c).
type LimitInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	Amount        string
	Currency      string
	ValidFrom     *time.Time
	ValidTo       *time.Time
	NextReview    *time.Time
}

// LimitChange is a merge patch of a credit limit: Amount nil leaves the
// amount, given in Currency otherwise; a date nil leaves it, and one
// pointing at nil clears it (T77c).
type LimitChange struct {
	Amount     *string
	Currency   string
	ValidFrom  **time.Time
	ValidTo    **time.Time
	NextReview **time.Time
}

// currencyMismatch refuses a limit in another currency than the
// company's: a limit is compared with what the customer owes in that
// currency, and nothing is converted (ADR-0066 §2).
func currencyMismatch(want string) *problem.Problem {
	return problem.Invalid("credit.currency_mismatch", "limit.currency", "A credit limit is in the company's currency, "+want+".")
}

// validity refuses a validity that ends before it starts (T77c); the
// database's credit_limit_validity_check holds the same.
func validity(from, to *time.Time) error {
	if from != nil && to != nil && to.Before(*from) {
		return problem.Invalid("credit.invalid_validity", "valid_to", "A credit limit's validity ends on its first day or later.")
	}
	return nil
}

// SetLimit sets a customer's credit limit in a company (ADR-0066 §2,
// T77a): one amount in the company's currency, used on the company's days
// within its validity (T77c), once its create change is approved (T77d):
// the limit is written pending, with its audit record, and the change
// asks for its approval; CreditLimitChanged follows when it applies. The
// customer is a business partner with the customer role and customer
// data in the company. A customer's second limit in a company is refused
// by the database, 409 credit.limit_exists.
func (e *Endpoints) SetLimit(ctx context.Context, in LimitInput) (Limit, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Limit{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Limit{}, fmt.Errorf("credit: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Limit{}, fmt.Errorf("credit: legal entity of a credit limit: %w", err)
	}
	c, ok, err := e.Customers.Customer(ctx, in.CustomerID, le.ID)
	switch {
	case err != nil:
		return Limit{}, fmt.Errorf("credit: customer %s of a credit limit: %w", in.CustomerID, err)
	case !ok:
		return Limit{}, problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	case !c.IsCustomer || !c.SetUp:
		return Limit{}, problem.Invalid("credit.customer_not_set_up", "customer_id", "The business partner is not a customer with customer data in company "+le.Code+".")
	case in.Currency != le.LocalCurrency:
		return Limit{}, currencyMismatch(le.LocalCurrency)
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil) // a legal entity's currency exists
	if err != nil {
		return Limit{}, fmt.Errorf("credit: currency of company %s: %w", le.Code, err)
	}
	amount, err := amountOf(in.Amount, in.Currency, exp, "limit.amount")
	if err != nil {
		return Limit{}, err
	}
	if err := validity(in.ValidFrom, in.ValidTo); err != nil {
		return Limit{}, err
	}
	// A check of one of the customer's orders that is under way ends first (hold).
	if err := hold(ctx, tx, le.ID, c.ID); err != nil {
		return Limit{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount,
			valid_from, valid_to, next_review_on, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', $11, $11)`,
		p.TenantID, id, le.ID, c.ID, c.Number, le.LocalCurrency, amount, in.ValidFrom, in.ValidTo, in.NextReview, p.ID); err != nil {
		return Limit{}, fmt.Errorf("credit: insert the credit limit of customer %s in company %s: %w", c.Number, le.Code, err)
	}
	l, err := limit(ctx, tx, id, true)
	if err != nil {
		return Limit{}, err
	}
	asked, err := e.request(ctx, tx, p, l, l.terms(), le)
	if err != nil {
		return Limit{}, err
	}
	return asked, audit.Record(ctx, "credit.credit_limit.create", "credit.credit_limit", l.ID, detailOf(l, nil, nil)) // its error names the action
}

// patched is the limit as the change would leave it: its amount, read in
// the limit's currency, and its dates, those the change leaves as they
// are.
func patched(l Limit, ch LimitChange) (Limit, error) {
	to := l
	if ch.Amount != nil {
		if ch.Currency != l.Currency {
			return Limit{}, currencyMismatch(l.Currency)
		}
		a, err := amountOf(*ch.Amount, ch.Currency, l.Exponent, "limit.amount")
		if err != nil {
			return Limit{}, err
		}
		to.Amount = a
	}
	for _, d := range []struct {
		patch **time.Time
		field **time.Time
	}{{ch.ValidFrom, &to.ValidFrom}, {ch.ValidTo, &to.ValidTo}, {ch.NextReview, &to.NextReview}} {
		if d.patch != nil {
			*d.field = *d.patch
		}
	}
	return to, validity(to.ValidFrom, to.ValidTo)
}

// unchanged says whether the limit after a change is the limit before.
func unchanged(before, after Limit) bool {
	return before.Amount == after.Amount && clock.SameDay(before.ValidFrom, after.ValidFrom) && clock.SameDay(before.ValidTo, after.ValidTo) &&
		clock.SameDay(before.NextReview, after.NextReview)
}

// raisesNothing says whether a change of an active limit applies at once
// (T77d): its amount is not raised and its validity stays, so the limit
// restricts no less; the review date may change with it. Any other
// change waits for its approval: a raise, and a change of the validity,
// which decides on which days the limit restricts at all.
func raisesNothing(before, after Limit) bool {
	return after.Amount <= before.Amount && clock.SameDay(before.ValidFrom, after.ValidFrom) && clock.SameDay(before.ValidTo, after.ValidTo)
}

// ChangeLimit changes the amount, the validity or the next review date
// of a credit limit with the caller's version of it (ADR-0008, ADR-0066
// §2, T77c): the row is locked and the version compared. A change of an
// active limit that raises nothing applies at once, recorded with what it
// changed before and after; any other, and any change of a pending limit,
// asks for its approval and answers the limit as it is (T77d). A limit
// with a change waiting is refused (409 credit.change_pending). The
// limit's customer, company and currency stay.
func (e *Endpoints) ChangeLimit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch LimitChange) (Limit, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Limit{}, err
	}
	l, err := limit(ctx, tx, id, true)
	if err != nil {
		return Limit{}, err
	}
	if err := ifMatch(l.Version); err != nil {
		return Limit{}, err
	}
	// Read again under the row lock, in a statement of its own: the locking
	// read's subquery may have seen a change that a decision committed
	// while the lock was awaited.
	if l, err = limit(ctx, tx, id, false); err != nil {
		return Limit{}, err
	}
	if l.PendingChangeID != nil {
		return Limit{}, errChangePending()
	}
	to, err := patched(l, ch)
	if err != nil {
		return Limit{}, err
	}
	// A pending limit is asked for again as it is too: its last request
	// was rejected or withdrawn.
	if l.Status == LimitActive && unchanged(l, to) {
		return Limit{}, problem.Unprocessable("credit.nothing_to_change", "The change would leave the credit limit as it is.")
	}
	le, err := e.companyOf(ctx, l.LegalEntityID)
	if err != nil {
		return Limit{}, err
	}
	if l.Status != LimitActive || !raisesNothing(l, to) {
		return e.request(ctx, tx, p, l, to.terms(), le)
	}
	after, err := writeTerms(ctx, tx, p, id, to.terms(), false)
	if err != nil {
		return Limit{}, err
	}
	return after, record(ctx, "credit.credit_limit.change", after, le, &l, nil)
}

// detailOf is a limit's audit detail: its customer, company, amount and
// dates, and, for a change, its amount and dates before (before nil: none,
// a limit set) and the credit limit change applied (change nil: none,
// one applied at once).
func detailOf(l Limit, before *Limit, change *uuid.UUID) map[string]any {
	detail := map[string]any{"legal_entity_id": l.LegalEntityID, "customer_id": l.CustomerID, "customer_number": l.CustomerNumber,
		"limit": money.EventAmount(l.Amount, l.Currency, l.Exponent), "limit_before": nil, "valid_from": outbox.Date(l.ValidFrom),
		"valid_to": outbox.Date(l.ValidTo), "next_review_on": outbox.Date(l.NextReview), "status": l.Status}
	if before != nil {
		detail["limit_before"] = money.EventAmount(before.Amount, l.Currency, l.Exponent)
		detail["valid_from_before"], detail["valid_to_before"] = outbox.Date(before.ValidFrom), outbox.Date(before.ValidTo)
		detail["next_review_on_before"] = outbox.Date(before.NextReview)
	}
	if change != nil {
		detail["credit_limit_change_id"] = *change
	}
	return detail
}

// record adds the audit record of a limit changed, or set in effect, and
// publishes CreditLimitChanged in the same transaction (ADR-0007,
// ADR-0011), both with the limit after and the limit before; before is
// nil for a limit that had none in effect, a create applied. change names
// the credit limit change applied, nil for a change applied at once
// (T77d). The event names the customer by id and number, never by name
// (product owner, 2026-11-05).
func record(ctx context.Context, action string, l Limit, le tenancy.Entity, before *Limit, change *uuid.UUID) error {
	detail := detailOf(l, before, change)
	payload := map[string]any{"credit_limit_id": l.ID, "legal_entity_id": l.LegalEntityID, "legal_entity_code": le.Code,
		"customer_id": l.CustomerID, "customer_number": l.CustomerNumber, "limit": detail["limit"], "limit_before": detail["limit_before"],
		"valid_from": detail["valid_from"], "valid_to": detail["valid_to"], "next_review_on": detail["next_review_on"],
		"credit_limit_change_id": change, "version": l.Version}
	if err := audit.Record(ctx, action, "credit.credit_limit", l.ID, detail,
		outbox.Event{Context: "credit", Name: "credit_limit_changed", AggregateType: "credit.credit_limit", AggregateID: l.ID, Payload: payload}); err != nil {
		return fmt.Errorf("credit: record %s of credit limit %s: %w", action, l.ID, err)
	}
	return nil
}
