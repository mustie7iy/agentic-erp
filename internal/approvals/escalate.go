package approvals

import (
	"context"
	"errors"
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

// EscalatorActor is the deadline workflow's id in audit trails and events.
var EscalatorActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:approval-escalator"))

// Escalate escalates the request when it is still pending and reports
// whether it was; a decided or withdrawn request is left alone, so the
// workflow ends.
func Escalate(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, now time.Time) (bool, error) {
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
		WHERE id = $1 AND status = 'pending' RETURNING object_type, object_id, current_step, created_at, escalations`, id, now).Scan(&r.objectType, &r.objectID, &r.step, &r.created, &r.count)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
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
