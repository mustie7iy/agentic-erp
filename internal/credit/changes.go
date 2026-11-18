package credit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Credit limit change kinds and statuses (ADR-0066, As built T77d).
const (
	KindCreate = "create" // a pending limit's approval
	KindChange = "change" // a change of an active limit

	ChangePending   = "pending"
	ChangeApplied   = "applied"
	ChangeRejected  = "rejected"
	ChangeCancelled = "cancelled"
)

func init() {
	problem.RegisterConstraint("credit_limit_change_one_pending", errChangePending)
}

// errChangePending refuses a change of a limit whose change waits for
// its approval: it is decided first.
func errChangePending() *problem.Problem {
	return problem.Conflict("credit.change_pending", "The credit limit has a change waiting for its approval; it is approved, rejected or withdrawn first.")
}

// Opener opens approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// Change is a row of credit.credit_limit_change: what a change asks of a
// limit (Amount in the limit's currency, the dates) and, for a change of
// an active limit, the limit as it was then (Before; nil for a create).
type Change struct {
	ID                uuid.UUID
	LimitID           uuid.UUID
	Kind              string
	Asked             Terms
	Before            *Terms
	Status            string
	WorkflowRequestID *uuid.UUID
	RequestedBy       uuid.UUID
	DecidedAt         *time.Time
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

// Terms are what a limit holds that a change sets: its amount and dates.
type Terms struct {
	Amount     money.Minor
	ValidFrom  *time.Time
	ValidTo    *time.Time
	NextReview *time.Time
}

// terms are the limit's terms.
func (l Limit) terms() Terms {
	return Terms{Amount: l.Amount, ValidFrom: l.ValidFrom, ValidTo: l.ValidTo, NextReview: l.NextReview}
}

const changeColumns = `c.id, c.credit_limit_id, c.kind, c.amount, c.valid_from, c.valid_to, c.next_review_on, c.amount_before, c.valid_from_before, c.valid_to_before,
	c.next_review_on_before, c.status, c.workflow_request_id, c.requested_by, c.decided_at, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by`

func scanChange(row pgx.Row) (Change, error) {
	var c Change
	var amount *money.Minor
	var before Terms
	err := row.Scan(&c.ID, &c.LimitID, &c.Kind, &c.Asked.Amount, &c.Asked.ValidFrom, &c.Asked.ValidTo, &c.Asked.NextReview, &amount,
		&before.ValidFrom, &before.ValidTo, &before.NextReview, &c.Status, &c.WorkflowRequestID, &c.RequestedBy, &c.DecidedAt, &c.Version,
		&c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	if amount != nil {
		before.Amount, c.Before = *amount, &before
	}
	return c, errs.Wrapf(err, "scan credit limit change")
}

// getChange reads a credit limit change; lock takes its row lock. Its
// limit's company access is the caller's to check.
func getChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Change, error) {
	q := `SELECT ` + changeColumns + ` FROM credit.credit_limit_change c WHERE c.id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	c, err := scanChange(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, problem.NotFoundOf("credit limit change")
	}
	if err != nil {
		return c, fmt.Errorf("credit: read credit limit change %s: %w", id, err)
	}
	return c, nil
}

// request asks for the change of l to the terms asked, a create when l
// is pending (ADR-0066, As built T77d): the change row, pending, its
// approval request by the fixed role, and the limit's version moved, since
// it now names the change. l is held under its row lock.
func (e *Endpoints) request(ctx context.Context, tx pgx.Tx, p reqctx.Principal, l Limit, asked Terms, le tenancy.Entity) (Limit, error) {
	// A create has no values before: its limit was never in effect, so
	// the before columns stay NULL.
	kind, was, wasAmount := KindCreate, Terms{}, (*money.Minor)(nil)
	if l.Status == LimitActive {
		kind, was = KindChange, l.terms()
		wasAmount = &was.Amount
	}
	c, err := scanChange(tx.QueryRow(ctx, `INSERT INTO credit.credit_limit_change AS c (tenant_id, id, credit_limit_id, kind, amount, valid_from, valid_to,
			next_review_on, amount_before, valid_from_before, valid_to_before, next_review_on_before, status, requested_by, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'pending', $13, $13, $13) RETURNING `+changeColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), l.ID, kind, asked.Amount, asked.ValidFrom, asked.ValidTo, asked.NextReview,
		wasAmount, was.ValidFrom, was.ValidTo, was.NextReview, p.ID))
	if err != nil {
		return Limit{}, fmt.Errorf("credit: ask for a %s of credit limit %s: %w", kind, l.ID, err)
	}
	currency := l.Currency
	wf, needed, err := e.Requests.Open(ctx, approvals.NewRequest{ObjectType: approvals.CreditLimitChange, ObjectID: c.ID,
		Title: fmt.Sprintf("Credit limit of customer %s in company %s: %s %s (%s)", l.CustomerNumber, le.Code,
			asked.Amount.Plain(l.Exponent), l.Currency, kind), Amount: &asked.Amount, Currency: &currency, LegalEntityID: l.LegalEntityID})
	if err != nil {
		return Limit{}, fmt.Errorf("credit: open the approval of credit limit change %s: %w", c.ID, err)
	}
	// The fixed role always asks one approval; a policy that asked none
	// would be a fault of the server's wiring, not of the request, so it
	// fails as one (500) rather than applying a limit unapproved.
	if !needed {
		return Limit{}, fmt.Errorf("credit: credit limit change %s: the approval policy asks no approval of it", c.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE credit.credit_limit_change SET workflow_request_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		c.ID, wf, p.ID); err != nil {
		return Limit{}, fmt.Errorf("credit: link credit limit change %s to approval request %s: %w", c.ID, wf, err)
	}
	if c, err = getChange(ctx, tx, c.ID, false); err != nil {
		return Limit{}, err
	}
	bumped, err := bumpVersion(ctx, tx, p, l.ID)
	if err != nil {
		return Limit{}, err
	}
	return bumped, recordChange(ctx, c, l, "credit.credit_limit_change.request")
}

// bumpVersion moves the limit's version, since what it shows changed (the
// change it names), and reads it back.
func bumpVersion(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID) (Limit, error) {
	if _, err := tx.Exec(ctx, `UPDATE credit.credit_limit SET version = version + 1, updated_by = $2 WHERE id = $1`, id, p.ID); err != nil {
		return Limit{}, fmt.Errorf("credit: move the version of credit limit %s: %w", id, err)
	}
	return limit(ctx, tx, id, false)
}

// lockPending locks a pending change's limit and the change, in the order a
// check of the customer's orders and a change of the limit take them
// (ADR-0008): the customer's key in the company (hold), the limit's row,
// then the change's; and requires the change pending (409
// credit.invalid_status). The limit is read as the caller may: one of a
// company the caller may not act in is not found. The change and its
// limit are read twice, first unlocked: the customer's key needs the
// limit's company and customer before any row lock is taken, and the
// locked reads then see what a decision that came first left.
func lockPending(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string) (Change, Limit, error) {
	c, err := getChange(ctx, tx, id, false)
	if err != nil {
		return Change{}, Limit{}, err
	}
	l, err := limit(ctx, tx, c.LimitID, false)
	if err != nil {
		return Change{}, Limit{}, err
	}
	if err := hold(ctx, tx, l.LegalEntityID, l.CustomerID); err != nil {
		return Change{}, Limit{}, err
	}
	if l, err = limit(ctx, tx, c.LimitID, true); err != nil {
		return Change{}, Limit{}, err
	}
	if c, err = getChange(ctx, tx, id, true); err != nil {
		return Change{}, Limit{}, err
	}
	if c.Status != ChangePending {
		return c, l, problem.Conflict("credit.invalid_status", "The credit limit change is "+c.Status+"; "+action+" requires it pending.")
	}
	return c, l, nil
}

// Apply applies an approved pending change (ADR-0066, As built T77d):
// the limit takes the terms asked for, and a pending limit becomes active,
// in effect from then on within its validity; audited, and published as
// CreditLimitChanged, with the limit before for a change of an active
// limit. The approvals context calls it when the change's request is
// approved.
func (e *Endpoints) Apply(ctx context.Context, id uuid.UUID) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	c, l, err := lockPending(ctx, tx, id, "applying")
	if err != nil {
		return Change{}, err
	}
	after, err := writeTerms(ctx, tx, p, l.ID, c.Asked, true)
	if err != nil {
		return Change{}, err
	}
	le, err := e.companyOf(ctx, l.LegalEntityID)
	if err != nil {
		return Change{}, err
	}
	var before *Limit
	if c.Kind == KindChange {
		before = &l
	}
	if err := record(ctx, "credit.credit_limit.change", after, le, before, &c.ID); err != nil {
		return Change{}, err
	}
	return closeChange(ctx, tx, p, c, after, ChangeApplied, "apply")
}

// Reject rejects a pending change: the limit stays as it is, a pending
// one pending and not in effect.
func (e *Endpoints) Reject(ctx context.Context, id uuid.UUID) (Change, error) {
	return e.decide(ctx, id, ChangeRejected, "reject")
}

// Cancel withdraws a pending change at its requester's request, as a
// rejection does.
func (e *Endpoints) Cancel(ctx context.Context, id uuid.UUID) (Change, error) {
	return e.decide(ctx, id, ChangeCancelled, "cancel")
}

func (e *Endpoints) decide(ctx context.Context, id uuid.UUID, status, action string) (Change, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Change{}, err
	}
	c, l, err := lockPending(ctx, tx, id, action)
	if err != nil {
		return Change{}, err
	}
	// The limit no longer names the change: its version moves.
	if l, err = bumpVersion(ctx, tx, p, l.ID); err != nil {
		return Change{}, err
	}
	return closeChange(ctx, tx, p, c, l, status, action)
}

// closeChange moves a pending change to its decision, with its audit
// record.
func closeChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Change, l Limit, status, action string) (Change, error) {
	closed, err := scanChange(tx.QueryRow(ctx, `UPDATE credit.credit_limit_change c SET status = $2, decided_at = now(), version = version + 1,
			updated_by = $3 WHERE id = $1 AND status = 'pending' RETURNING `+changeColumns, c.ID, status, p.ID))
	if err != nil {
		return Change{}, fmt.Errorf("credit: %s credit limit change %s: %w", action, c.ID, err)
	}
	return closed, recordChange(ctx, closed, l, "credit.credit_limit_change."+action)
}

// recordChange adds the audit record of a change's step (ADR-0011): its
// limit, customer and company, kind and status, and the amount asked for.
func recordChange(ctx context.Context, c Change, l Limit, action string) error {
	detail := map[string]any{"credit_limit_id": l.ID, "legal_entity_id": l.LegalEntityID, "customer_id": l.CustomerID,
		"customer_number": l.CustomerNumber, "kind": c.Kind, "status": c.Status, "workflow_request_id": c.WorkflowRequestID,
		"limit": money.EventAmount(c.Asked.Amount, l.Currency, l.Exponent), "valid_from": outbox.Date(c.Asked.ValidFrom),
		"valid_to": outbox.Date(c.Asked.ValidTo), "next_review_on": outbox.Date(c.Asked.NextReview)}
	return audit.Record(ctx, action, "credit.credit_limit_change", c.ID, detail) // its error names the action
}

// Approvals moves credit limit changes on when their approval requests
// complete (approvals.Handler, approvals.Locator).
type Approvals struct{ Endpoints *Endpoints }

// Approved applies the change.
func (a Approvals) Approved(ctx context.Context, change, _ uuid.UUID) error {
	_, err := a.Endpoints.Apply(ctx, change)
	return err
}

// Rejected rejects the change.
func (a Approvals) Rejected(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.Reject(ctx, change)
	return err
}

// Cancelled withdraws the change: its requester cancelled its request.
func (a Approvals) Cancelled(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.Cancel(ctx, change)
	return err
}

// Resources is where the API reads each credit limit change.
func (Approvals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/credit-limit-changes/%s", ids), nil
}
