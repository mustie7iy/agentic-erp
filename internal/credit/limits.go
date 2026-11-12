package credit

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LimitInput is a credit limit as a client sets it: the amount as the
// request gives it, a decimal string in Currency.
type LimitInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	Amount        string
	Currency      string
}

// currencyMismatch refuses a limit in another currency than the
// company's: a limit is compared with what the customer owes in that
// currency, and nothing is converted (ADR-0066 §2).
func currencyMismatch(want string) *problem.Problem {
	return problem.Invalid("credit.currency_mismatch", "limit.currency", "A credit limit is in the company's currency, "+want+".")
}

// SetLimit sets a customer's credit limit in a company (ADR-0066 §2,
// T77a): one amount in the company's currency, holding at once, with its
// audit record and CreditLimitChanged. The customer is a business partner
// with the customer role and customer data in the company. A customer's
// second limit in a company is refused by the database, 409
// credit.limit_exists.
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
	// A check of one of the customer's orders that is under way ends first (hold).
	if err := hold(ctx, tx, le.ID, c.ID); err != nil {
		return Limit{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		p.TenantID, id, le.ID, c.ID, c.Number, le.LocalCurrency, amount, p.ID); err != nil {
		return Limit{}, fmt.Errorf("credit: insert the credit limit of customer %s in company %s: %w", c.Number, le.Code, err)
	}
	l, err := limit(ctx, tx, id, false)
	if err != nil {
		return Limit{}, err
	}
	return l, record(ctx, "credit.credit_limit.create", l, le, nil)
}

// ChangeLimit changes the amount of a credit limit with the caller's
// version of it (ADR-0008, ADR-0066 §2): the row is locked, the version
// compared, and the change recorded with the amounts before and after.
// The limit's customer, company and currency stay.
func (e *Endpoints) ChangeLimit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, amount, code string) (Limit, error) {
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
	if code != l.Currency {
		return Limit{}, currencyMismatch(l.Currency)
	}
	to, err := amountOf(amount, code, l.Exponent, "limit.amount")
	if err != nil {
		return Limit{}, err
	}
	if to == l.Amount {
		return Limit{}, problem.Unprocessable("credit.nothing_to_change", "The change would leave the credit limit as it is.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, l.LegalEntityID) // a stored limit's legal entity exists
	if err != nil {
		return Limit{}, fmt.Errorf("credit: company of credit limit %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE credit.credit_limit SET amount = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, to, p.ID); err != nil {
		return Limit{}, fmt.Errorf("credit: change credit limit %s: %w", id, err)
	}
	after, err := limit(ctx, tx, id, false)
	if err != nil {
		return Limit{}, err
	}
	return after, record(ctx, "credit.credit_limit.change", after, le, &l.Amount)
}

// record adds the audit record of a limit set or changed and publishes
// CreditLimitChanged in the same transaction (ADR-0007, ADR-0011), both
// with the limit after and the limit before; before is nil for a limit
// just set, which had none. The event names the customer by id and
// number, never by name (product owner, 2026-11-05).
func record(ctx context.Context, action string, l Limit, le tenancy.Entity, before *money.Minor) error {
	var was any
	if before != nil {
		was = money.EventAmount(*before, l.Currency, l.Exponent)
	}
	now := money.EventAmount(l.Amount, l.Currency, l.Exponent)
	detail := map[string]any{"legal_entity_id": l.LegalEntityID, "customer_id": l.CustomerID, "customer_number": l.CustomerNumber,
		"limit": now, "limit_before": was}
	payload := map[string]any{"credit_limit_id": l.ID, "legal_entity_id": l.LegalEntityID, "legal_entity_code": le.Code,
		"customer_id": l.CustomerID, "customer_number": l.CustomerNumber, "limit": now, "limit_before": was, "version": l.Version}
	if err := audit.Record(ctx, action, "credit.credit_limit", l.ID, detail,
		outbox.Event{Context: "credit", Name: "credit_limit_changed", AggregateType: "credit.credit_limit", AggregateID: l.ID, Payload: payload}); err != nil {
		return fmt.Errorf("credit: record %s of credit limit %s: %w", action, l.ID, err)
	}
	return nil
}
