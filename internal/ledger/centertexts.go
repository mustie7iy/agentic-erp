package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TextChange is a merge patch of texts: nil fields are unchanged; a
// Description pointing to nil clears it.
type TextChange struct {
	Name        *string
	Description **string
	Responsible *string
}

// fields names the texts the patch changes, for the audit trail: the
// values may name a person (the responsible) and stay out of it.
func (ch TextChange) fields() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"name", ch.Name != nil}, {"description", ch.Description != nil}, {"responsible", ch.Responsible != nil}} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// updateTexts changes a center's texts, which need no approval (ADR-0050
// §11, decision P1); the caller records which (the values may name a
// person and stay out of the audit trail). A rejected center is not
// changed any more.
func updateTexts(ctx context.Context, tx pgx.Tx, k centerKind, h centerHead, ifMatch func(int32) error, ch TextChange) error {
	if err := ifMatch(h.Version); err != nil {
		return err
	}
	if h.Status == CenterRejected {
		return problem.Conflict("ledger.invalid_center_status", "The center is rejected; it is not changed any more.")
	}
	if len(ch.fields()) == 0 {
		return nothingToChange()
	}
	var desc *string
	if ch.Description != nil {
		desc = *ch.Description
	}
	_, err := tx.Exec(ctx, `UPDATE `+k.ident(k.table)+` SET name = coalesce($3, name), description = CASE WHEN $4 THEN $5 ELSE description END,
		responsible = coalesce($6, responsible), version = version + 1, updated_by = $2 WHERE id = $1`,
		h.ID, actor(ctx), ch.Name, ch.Description != nil, desc, ch.Responsible)
	return errs.Wrapf(err, "ledger: update texts of %s %s", k.table, h.ID)
}

// NewSegment is a segment to create.
type NewSegment struct {
	Code, Name  string
	Description *string
}

// CreateSegment creates a segment at once: it changes nothing until a
// profit center names it, which is an approved change.
func CreateSegment(ctx context.Context, in NewSegment) (Segment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Segment{}, err
	}
	s, err := scanSegment(tx.QueryRow(ctx, `INSERT INTO ledger.segment (tenant_id, id, code, name, description, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+segmentColumns, p.TenantID, uuid.Must(uuid.NewV7()), in.Code, in.Name, in.Description, p.ID))
	if err != nil {
		return Segment{}, fmt.Errorf("ledger: insert segment %s: %w", in.Code, err)
	}
	return s, recordSegment(ctx, s, "create")
}

// UpdateSegment changes a segment's name or description.
func UpdateSegment(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch TextChange) (Segment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Segment{}, err
	}
	s, err := getSegment(ctx, tx, id, true)
	if err != nil {
		return Segment{}, err
	}
	if err := ifMatch(s.Version); err != nil {
		return Segment{}, err
	}
	if len(ch.fields()) == 0 {
		return Segment{}, nothingToChange()
	}
	var desc *string
	if ch.Description != nil {
		desc = *ch.Description
	}
	s, err = scanSegment(tx.QueryRow(ctx, `UPDATE ledger.segment SET name = coalesce($2, name), description = CASE WHEN $3 THEN $4 ELSE description END,
		version = version + 1, updated_by = $5 WHERE id = $1 RETURNING `+segmentColumns, id, ch.Name, ch.Description != nil, desc, actor(ctx)))
	if err != nil {
		return Segment{}, fmt.Errorf("ledger: update segment %s: %w", id, err)
	}
	return s, recordSegment(ctx, s, "update")
}

func recordSegment(ctx context.Context, s Segment, action string) error {
	change := map[string]string{"create": "created", "update": "updated"}[action]
	return audit.Record(ctx, "ledger.segment."+action, "ledger.segment", s.ID, map[string]any{"code": s.Code, "name": s.Name},
		outbox.Event{Context: "ledger", Name: "segment_changed", AggregateType: "ledger.segment", AggregateID: s.ID,
			Payload: map[string]any{"segment_id": s.ID, "code": s.Code, "name": s.Name, "change": change}})
}
