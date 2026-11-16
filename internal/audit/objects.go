package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ObjectRef is an object of the trail: its type and id.
type ObjectRef struct {
	Type string
	ID   uuid.UUID
}

// ObjectEvent is what an event log reads of an audit event (T103a,
// ADR-0080): its sequence number, time, the kind of its actor, its
// action and object; never who the actor was.
type ObjectEvent struct {
	Seq        int64
	OccurredAt time.Time
	ActorKind  string
	Action     string
	Object     ObjectRef
}

// Trail reads the audit events about objects for the processmining
// context's event logs. It reads in the caller's transaction.
type Trail struct{}

// About returns the events about the objects whose action is one of the
// actions, by sequence number.
func (Trail) About(ctx context.Context, objects []ObjectRef, actions []string) ([]ObjectEvent, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	types, ids := make([]string, len(objects)), make([]uuid.UUID, len(objects))
	for i, o := range objects {
		types[i], ids[i] = o.Type, o.ID
	}
	rows, _ := tx.Query(ctx, `SELECT e.seq, e.occurred_at, e.actor_kind, e.action, e.object_type, e.object_id
		FROM unnest($1::text[], $2::uuid[]) o(type, id)
		JOIN audit.audit_event e ON e.object_type = o.type AND e.object_id = o.id
		WHERE e.action = ANY($3) ORDER BY e.seq`, types, ids, actions)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ObjectEvent, error) {
		var v ObjectEvent
		err := r.Scan(&v.Seq, &v.OccurredAt, &v.ActorKind, &v.Action, &v.Object.Type, &v.Object.ID)
		return v, errs.Wrapf(err, "scan an audit event")
	})
	if err != nil {
		return nil, fmt.Errorf("audit: the events of %d objects: %w", len(objects), err)
	}
	return out, nil
}
