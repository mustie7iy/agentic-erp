package approvals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Approval deadlines (T29, ADR-0012): a pending request that gets no
// decision within the escalation period is escalated by its deadline
// workflow, as a system actor: the request counts the escalation, ApprovalEscalated
// is published for the inbox and notification read models, and the audit
// trail records it.

// ErrEscalationCount reports a pending request with fewer escalations than
// its workflow counted: a state no retry can repair.
var ErrEscalationCount = errors.New("approvals: pending with fewer escalations than the workflow counted")

// EscalatorActor is the deadline workflow's id in audit trails and events.
var EscalatorActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:approval-escalator"))

// Escalate escalates the request when it is still pending and reports
// whether it still is; a decided or withdrawn request is left alone, so the
// workflow ends. prior is the number of escalations the workflow has seen
// before this one: the update is guarded by it, so a retry after a commit
// whose completion was lost finds the escalation done and reports the
// request pending without counting, publishing or auditing it again
// (ADR-0008, ADR-0012). A nil prior comes from an activity scheduled before
// the guard existed and escalates unguarded, as it did then.
func Escalate(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, prior *int16, now time.Time) (bool, error) {
	if err := database.SetActor(ctx, tx, EscalatorActor); err != nil {
		return false, err
	}
	var r struct {
		objectType string
		objectID   uuid.UUID
		step       int16
		created    time.Time
		count      int16
	}
	err := tx.QueryRow(ctx, `UPDATE workflow.approval_request SET escalations = escalations + 1, escalated_at = $2, version = version + 1
		WHERE id = $1 AND status = 'pending' AND ($3::smallint IS NULL OR escalations = $3)
		RETURNING object_type, object_id, current_step, created_at, escalations`, id, now, prior).Scan(&r.objectType, &r.objectID, &r.step, &r.created, &r.count)
	if errors.Is(err, pgx.ErrNoRows) {
		return escalatedBefore(ctx, tx, id, prior)
	}
	if err != nil {
		return false, fmt.Errorf("approvals: escalate request %s: %w", id, err)
	}
	if _, err := outbox.Write(ctx, tx, tenant, EscalatorActor, "", outbox.Event{Context: "workflow", Name: "approval_escalated", AggregateType: "workflow.approval_request", AggregateID: id,
		Payload: map[string]any{"approval_request_id": id, "object_type": r.objectType, "object_id": r.objectID, "current_step": r.step, "escalations": r.count,
			"pending_since": r.created.UTC().Format(time.RFC3339Nano)}}); err != nil {
		return false, err
	}
	actor := reqctx.Principal{ID: EscalatorActor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "workflow.approval_request.escalate", ObjectType: "workflow.approval_request", ObjectID: id,
		Outcome: "succeeded", Detail: map[string]any{"escalations": r.count, "current_step": r.step}}); err != nil {
		return false, err
	}
	return true, nil
}

// escalatedBefore explains a guarded escalation that changed no row: the
// request is gone or no longer pending (false, the workflow ends), or this
// escalation was already made by an earlier run of the activity (true, the
// workflow goes on). A pending request with fewer escalations than the
// workflow counted cannot be explained and is an error.
func escalatedBefore(ctx context.Context, tx pgx.Tx, id uuid.UUID, prior *int16) (bool, error) {
	var status string
	var count int16
	err := tx.QueryRow(ctx, `SELECT status, escalations FROM workflow.approval_request WHERE id = $1`, id).Scan(&status, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("approvals: escalate request %s: read it: %w", id, err)
	}
	if status != "pending" {
		return false, nil
	}
	if prior != nil && count > *prior {
		return true, nil
	}
	return false, fmt.Errorf("escalate request %s with %d escalations: %w", id, count, ErrEscalationCount)
}

// Ref names a request across tenants.
type Ref struct{ TenantID, ID uuid.UUID }

// Unwatched lists, across tenants, the pending requests without a deadline
// workflow, oldest first: what the workflows dispatcher asks approvals, as
// the worker role, so that it reads none of its tables.
func Unwatched(ctx context.Context, tx pgx.Tx) ([]Ref, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id FROM workflow.approval_request WHERE status = 'pending' AND workflow_id IS NULL ORDER BY id`)
	pending, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Ref])
	if err != nil {
		return nil, fmt.Errorf("approvals: list the pending requests without a deadline workflow: %w", err)
	}
	return pending, nil
}

// Watched records the deadline workflow that watches the request.
func Watched(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, workflowID string) error {
	if _, err := tx.Exec(ctx, `UPDATE workflow.approval_request SET workflow_id = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		tenant, id, workflowID); err != nil {
		return fmt.Errorf("approvals: record the deadline workflow of request %s: %w", id, err)
	}
	return nil
}
