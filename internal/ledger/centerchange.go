package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A center change (ADR-0050 §11, decision P1) asks for what only a
// controller may decide: creating a center, a new validity, locks, a move
// to another profit center, category or segment, more companies. It is
// opened with its center_change approval request and applied in the
// deciding transaction (CenterChanges); what it asks for is planned, and
// checked, both when it is requested and when it is applied.

// Center change kinds.
const (
	KindCreate = "create"
	KindRedate = "redate"
	KindLock   = "lock"
	KindMove   = "move"
	KindAssign = "assign"
)

// Center change statuses.
const (
	ChangePending   = "pending"
	ChangeApplied   = "applied"
	ChangeRejected  = "rejected"
	ChangeCancelled = "cancelled"
)

// CenterChange is a row of ledger.center_change.
type CenterChange struct {
	ID                uuid.UUID
	CostCenterID      *uuid.UUID
	ProfitCenterID    *uuid.UUID
	Kind              string
	Payload           map[string]any
	Reason            string
	Status            string
	WorkflowRequestID *uuid.UUID
	RequestedBy       uuid.UUID
	AppliedAt         *time.Time
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const changeColumns = `ch.id, ch.cost_center_id, ch.profit_center_id, ch.kind, ch.payload, ch.reason, ch.status, ch.workflow_request_id,
	ch.requested_by, ch.applied_at, ch.version, ch.created_at, ch.created_by, ch.updated_at, ch.updated_by`

func scanChange(row pgx.Row) (CenterChange, error) {
	var c CenterChange
	err := row.Scan(&c.ID, &c.CostCenterID, &c.ProfitCenterID, &c.Kind, &c.Payload, &c.Reason, &c.Status, &c.WorkflowRequestID,
		&c.RequestedBy, &c.AppliedAt, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan center change")
}

// visibleChanges limits changes to those of profit centers and of cost
// centers in the companies of access ($1 all, $2 the ids).
const visibleChanges = `(ch.cost_center_id IS NULL OR $1 OR EXISTS (SELECT FROM ledger.cost_center c
	WHERE c.tenant_id = ch.tenant_id AND c.id = ch.cost_center_id AND c.legal_entity_id = ANY($2)))`

func getChange(ctx context.Context, tx pgx.Tx, access authz.Access, id uuid.UUID, lock bool) (CenterChange, error) {
	q := `SELECT ` + changeColumns + ` FROM ledger.center_change ch WHERE ` + visibleChanges + ` AND ch.id = $3`
	if lock {
		q += ` FOR UPDATE OF ch`
	}
	c, err := scanChange(tx.QueryRow(ctx, q, access.All, access.IDs, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return CenterChange{}, problem.NotFoundOf("center change")
	}
	if err != nil {
		return CenterChange{}, fmt.Errorf("ledger: read center change %s: %w", id, err)
	}
	return c, nil
}

// centerRef names the center a change is for.
type centerRef struct {
	kind  centerKind
	id    uuid.UUID
	title string // how the approval request names the center
}

// centerKind is what differs between cost and profit centers in changes.
type centerKind struct {
	table, periods, object string // tables and the audit's object type
}

var (
	costCenters   = centerKind{table: "cost_center", periods: "cost_center_period", object: "ledger.cost_center"}
	profitCenters = centerKind{table: "profit_center", periods: "profit_center_period", object: "ledger.profit_center"}
)

func (k centerKind) ident(table string) string { return pgx.Identifier{"ledger", table}.Sanitize() }

// touch records a change of the center's representation (its periods, its
// pending change or its status): the version, which is its ETag, moves on.
func (k centerKind) touch(ctx context.Context, tx pgx.Tx, id uuid.UUID, set string, args ...any) error {
	_, err := tx.Exec(ctx, `UPDATE `+k.ident(k.table)+` SET `+set+`version = version + 1, updated_by = $2 WHERE id = $1`, append([]any{id, actor(ctx)}, args...)...)
	return errs.Wrapf(err, "ledger: update %s %s", k.table, id)
}

// principal is the request's principal; callers have checked there is
// one (reqctx.Request).
func principal(ctx context.Context) reqctx.Principal {
	p, _ := reqctx.PrincipalFrom(ctx) // every caller runs inside the unit of work
	return p
}

func actor(ctx context.Context) uuid.UUID { return principal(ctx).ID }

// requestChange records a pending change of the center and opens its
// approval request. The center is locked by the caller.
func requestChange(ctx context.Context, tx pgx.Tx, opener Opener, ref centerRef, kind string, payload any, reason string) (CenterChange, error) {
	p := principal(ctx)
	var cc, pc *uuid.UUID
	if ref.kind == costCenters {
		cc = &ref.id
	} else {
		pc = &ref.id
	}
	b, _ := json.Marshal(payload) // the payloads are structs of strings, ids, dates and flags
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, profit_center_id, kind, payload, reason, status,
			requested_by, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $8, $8)`,
		p.TenantID, id, cc, pc, kind, b, reason, p.ID); err != nil {
		return CenterChange{}, fmt.Errorf("ledger: insert %s change of %s %s: %w", kind, ref.kind.table, ref.id, err)
	}
	request, needed, err := opener.Open(ctx, approvals.NewRequest{ObjectType: approvals.CenterChange, ObjectID: id,
		Title: fmt.Sprintf("%s: %s: %s", ref.title, kind, reason)})
	if err != nil {
		return CenterChange{}, err
	}
	if !needed {
		return CenterChange{}, fmt.Errorf("ledger: %s change of %s %s: the approval policy requires no approval, which it always must", kind, ref.kind.table, ref.id)
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.center_change SET workflow_request_id = $2, version = version + 1 WHERE id = $1`, id, request); err != nil {
		return CenterChange{}, fmt.Errorf("ledger: link center change %s to approval request %s: %w", id, request, err)
	}
	if err := ref.kind.touch(ctx, tx, ref.id, ""); err != nil {
		return CenterChange{}, err
	}
	c, err := getChange(ctx, tx, authz.Access{All: true}, id, false)
	if err != nil {
		return CenterChange{}, err
	}
	return c, audit.Add(ctx, audit.Entry{Action: ref.kind.object + ".request", ObjectType: ref.kind.object, ObjectID: ref.id, Outcome: "succeeded",
		Detail: map[string]any{"kind": kind, "center_change_id": id, "workflow_request_id": request, "payload": c.Payload, "reason": reason}})
}

// decided locks the change and checks that it is pending and decided on
// the request: a request that is not the change's fails the decision. The
// center is locked first, as a request for a change locks it before it
// inserts one, so a decision and a new request on the center queue rather
// than deadlock.
func decided(ctx context.Context, tx pgx.Tx, id, request uuid.UUID) (CenterChange, error) {
	all := authz.Access{All: true}
	c, err := getChange(ctx, tx, all, id, false)
	if err != nil {
		return CenterChange{}, err
	}
	k, center := profitCenters, c.ProfitCenterID
	if c.CostCenterID != nil {
		k, center = costCenters, c.CostCenterID
	}
	if _, err := tx.Exec(ctx, `SELECT FROM `+k.ident(k.table)+` WHERE id = $1 FOR UPDATE`, *center); err != nil {
		return CenterChange{}, fmt.Errorf("ledger: lock %s %s of center change %s: %w", k.table, *center, id, err)
	}
	if c, err = getChange(ctx, tx, all, id, true); err != nil {
		return CenterChange{}, err
	}
	if c.Status != ChangePending || c.WorkflowRequestID == nil || *c.WorkflowRequestID != request {
		return CenterChange{}, fmt.Errorf("ledger: center change %s (%s) is not pending on approval request %s", id, c.Status, request)
	}
	return c, nil
}

// CenterChanges moves center changes on when their approval requests
// complete (approvals.Handler), in the deciding transaction. The decider
// may not see the cost center's company: the request is what authorises
// the change, so the center is read without the company filter.
type CenterChanges struct{}

// Approved applies the change.
func (CenterChanges) Approved(ctx context.Context, change, request uuid.UUID) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := decided(ctx, tx, change, request)
	if err != nil {
		return err
	}
	if c.CostCenterID != nil {
		err = applyCostCenterChange(ctx, tx, c)
	} else {
		err = applyProfitCenterChange(ctx, tx, c)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE ledger.center_change SET status = 'applied', applied_at = now(), version = version + 1, updated_by = $2 WHERE id = $1`,
		change, actor(ctx))
	return errs.Wrapf(err, "ledger: mark center change %s applied", change)
}

// Rejected rejects the change; a rejected create rejects its center.
func (CenterChanges) Rejected(ctx context.Context, change, request uuid.UUID, comment string) error {
	return closeChange(ctx, change, request, ChangeRejected, comment)
}

// Cancelled cancels the change: its requester withdrew it. A withdrawn
// create rejects its center.
func (CenterChanges) Cancelled(ctx context.Context, change, request uuid.UUID, comment string) error {
	return closeChange(ctx, change, request, ChangeCancelled, comment)
}

func closeChange(ctx context.Context, id, request uuid.UUID, status, comment string) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := decided(ctx, tx, id, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.center_change SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, status, actor(ctx)); err != nil {
		return fmt.Errorf("ledger: close center change %s as %s: %w", id, status, err)
	}
	if c.CostCenterID != nil {
		err = closedCostCenter(ctx, tx, c)
	} else {
		err = closedProfitCenter(ctx, tx, c)
	}
	if err != nil {
		return err
	}
	return audit.Add(ctx, audit.Entry{Action: "ledger.center_change." + map[string]string{ChangeRejected: "reject", ChangeCancelled: "cancel"}[status],
		ObjectType: "ledger.center_change", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"kind": c.Kind, "cost_center_id": c.CostCenterID, "profit_center_id": c.ProfitCenterID, "comment": comment}})
}

// decode reads a stored payload into the struct it was written from.
func decode(payload map[string]any, into any) {
	b, _ := json.Marshal(payload) // stored payloads are JSON objects
	_ = json.Unmarshal(b, into)   // written from the same structs (requestChange)
}

// dateText is a date as payloads and events carry it; "" for none.
func dateText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

// parseDate reads a payload's date; "" is the zero time (open-ended).
func parseDate(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s) // written by dateText; "" parses as the zero time
	return t
}

// datePtr is a date for an event, nil when open-ended.
func datePtr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := dateText(t)
	return &s
}
