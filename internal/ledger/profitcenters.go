package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// profitCenterPayload is what a profit center change asks for; each kind
// uses its fields, as costCenterPayload.
type profitCenterPayload struct {
	ValidFrom      string      `json:"valid_from,omitempty"`
	ValidTo        string      `json:"valid_to,omitempty"`
	Locked         *bool       `json:"locked,omitempty"`
	LegalEntityIDs []uuid.UUID `json:"legal_entity_ids,omitempty"` // create, assign
	StandardNodeID *uuid.UUID  `json:"standard_node_id,omitempty"` // create: its leaf in the standard hierarchy (T56b)
	// Segment is a move's new segment, its id null for none.
	Segment *segmentRef `json:"segment,omitempty"`
}

type segmentRef struct {
	ID *uuid.UUID `json:"id"`
}

// NewProfitCenter is a profit center to create.
type NewProfitCenter struct {
	Code           string
	Texts          Texts
	SegmentID      *uuid.UUID
	LegalEntityIDs []uuid.UUID
	StandardNodeID *uuid.UUID // its leaf in the standard hierarchy; nil: the UNASSIGNED leaf, at approval (unassignedLeaf)
	ValidFrom      time.Time
	ValidTo        time.Time // zero: open-ended
	Reason         string
}

// accessible checks that the caller may act in each legal entity.
func (e *Endpoints) accessible(ctx context.Context, ids []uuid.UUID) error {
	for i, id := range ids {
		if _, err := e.legalEntity(ctx, id, fmt.Sprintf("legal_entity_ids[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// checkSegment checks that a named segment exists.
func checkSegment(ctx context.Context, tx pgx.Tx, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	_, err := getSegment(ctx, tx, *id, false)
	return problem.Reference(err, "segment_id", "segment", "a segment of the tenant")
}

// RequestProfitCenter creates a pending profit center and its create
// change, with its approval request.
func (e *Endpoints) RequestProfitCenter(ctx context.Context, in NewProfitCenter) (ProfitCenter, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ProfitCenter{}, err
	}
	if err := e.accessible(ctx, in.LegalEntityIDs); err != nil {
		return ProfitCenter{}, err
	}
	if err := checkSegment(ctx, tx, in.SegmentID); err != nil {
		return ProfitCenter{}, err
	}
	in.ValidFrom, in.ValidTo = day(in.ValidFrom), dayOrOpen(in.ValidTo)
	if err := checkDates(in.ValidFrom, in.ValidTo); err != nil {
		return ProfitCenter{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.profit_center (tenant_id, id, code, segment_id, name, description, responsible, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $8)`, p.TenantID, id, in.Code, in.SegmentID, in.Texts.Name, in.Texts.Description, in.Texts.Responsible, p.ID); err != nil {
		return ProfitCenter{}, fmt.Errorf("ledger: insert profit center %s: %w", in.Code, err)
	}
	if in.StandardNodeID != nil {
		if err := checkStandardNode(ctx, tx, HierarchyProfitCenters, id, *in.StandardNodeID); err != nil {
			return ProfitCenter{}, err
		}
	}
	payload := profitCenterPayload{ValidFrom: dateText(in.ValidFrom), ValidTo: dateText(in.ValidTo), LegalEntityIDs: in.LegalEntityIDs,
		StandardNodeID: in.StandardNodeID}
	if _, err := requestChange(ctx, tx, e.Approvals, profitCenterRef(id, in.Code), KindCreate, payload, in.Reason); err != nil {
		return ProfitCenter{}, err
	}
	return getProfitCenter(ctx, tx, id, noLock)
}

func profitCenterRef(id uuid.UUID, code string) centerRef {
	return centerRef{kind: profitCenters, id: id, title: "Profit center " + code}
}

// RequestProfitCenterChange opens a change of an active profit center: a
// new validity, a lock, another segment or more companies. The payload is
// built from the center's validity, as for cost centers.
func (e *Endpoints) RequestProfitCenterChange(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, kind string,
	build func(from, to time.Time) profitCenterPayload, reason string) (CenterChange, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CenterChange{}, err
	}
	c, err := getProfitCenter(ctx, tx, id, forUpdate)
	if err != nil {
		return CenterChange{}, err
	}
	if err := changeable(c.centerHead, ifMatch); err != nil {
		return CenterChange{}, err
	}
	from, to, _ := validity(c.Periods)
	payload := build(from, to)
	if err := e.accessible(ctx, payload.LegalEntityIDs); err != nil {
		return CenterChange{}, err
	}
	if _, err := planProfitCenter(ctx, tx, c, kind, payload); err != nil {
		return CenterChange{}, err
	}
	return requestChange(ctx, tx, e.Approvals, profitCenterRef(id, c.Code), kind, payload, reason)
}

// planProfitCenter computes the profit center's periods after the change
// and checks it; it refuses a change that changes nothing.
func planProfitCenter(ctx context.Context, tx pgx.Tx, c ProfitCenter, kind string, p profitCenterPayload) ([]span[ProfitCenterAttrs], error) {
	from, to := parseDate(p.ValidFrom), parseDate(p.ValidTo)
	switch kind {
	case KindCreate:
		return []span[ProfitCenterAttrs]{{From: from, To: to}}, nil
	case KindMove:
		if err := checkSegment(ctx, tx, p.Segment.ID); err != nil {
			return nil, err
		}
		if equalIDs(p.Segment.ID, c.SegmentID) {
			return nil, nothingToChange()
		}
		return c.Periods, nil
	case KindAssign:
		if len(newCompanies(c, p.LegalEntityIDs)) == 0 {
			return nil, nothingToChange()
		}
		return c.Periods, nil
	}
	var spans []span[ProfitCenterAttrs]
	var err error
	if kind == KindLock {
		spans, err = lockedSpans(c, from, to, *p.Locked)
	} else {
		spans, err = redatedProfitCenter(ctx, tx, c, from, to)
	}
	if err != nil {
		return nil, err
	}
	if _, _, differ := diffSpans(c.Periods, spans); !differ {
		return nil, nothingToChange()
	}
	return spans, keepsDefaultProfitCenter(ctx, tx, c.ID, spans)
}

func equalIDs(a, b *uuid.UUID) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// newCompanies are the legal entities not yet assigned.
func newCompanies(c ProfitCenter, ids []uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, id := range ids {
		if !slices.Contains(c.LegalEntityIDs, id) {
			out = append(out, id)
		}
	}
	return out
}

func lockedSpans(c ProfitCenter, from, to time.Time, locked bool) ([]span[ProfitCenterAttrs], error) {
	if to.IsZero() {
		_, to, _ = validity(c.Periods)
	}
	if err := checkDates(from, to); err != nil {
		return nil, err
	}
	if !within(c.Periods, from, to) {
		return nil, invalidValidity("valid_from", "the lock must lie within the profit center's validity")
	}
	return setFrom(c.Periods, from, to, func(a ProfitCenterAttrs) ProfitCenterAttrs { a.Locked = locked; return a }), nil
}

// redatedProfitCenter gives the profit center a new validity, which must
// still cover every active cost center period naming it.
func redatedProfitCenter(ctx context.Context, tx pgx.Tx, c ProfitCenter, from, to time.Time) ([]span[ProfitCenterAttrs], error) {
	if err := checkDates(from, to); err != nil {
		return nil, err
	}
	spans, ok := redated(c.Periods, from, to)
	if !ok {
		return nil, invalidValidity("valid_from", "the new validity does not overlap the current one")
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.cost_center_period WHERE profit_center_id = $1 AND retired_by_change_id IS NULL
		AND (valid_from < $2 OR coalesce(valid_to, 'infinity') > coalesce($3::date, 'infinity')))`, c.ID, from, nullDate(to)).Scan(&used); err != nil {
		return nil, fmt.Errorf("ledger: cost centers naming profit center %s: %w", c.ID, err)
	}
	if used {
		return nil, problem.Conflict("ledger.profit_center_in_use", "Cost centers name the profit center outside the new validity; move them first.")
	}
	return spans, nil
}

// applyProfitCenterChange applies an approved change of a profit center.
func applyProfitCenterChange(ctx context.Context, tx pgx.Tx, ch CenterChange) error {
	c, err := getProfitCenter(ctx, tx, *ch.ProfitCenterID, forUpdate)
	if err != nil {
		return err
	}
	var p profitCenterPayload
	decode(ch.Payload, &p)
	spans, err := planProfitCenter(ctx, tx, c, ch.Kind, p)
	if err != nil {
		return err
	}
	if err := writeProfitCenter(ctx, tx, c, ch, p, spans); err != nil {
		return err
	}
	switch ch.Kind {
	case KindCreate:
		err = activate(ctx, tx, profitCenters, c.ID, ch.ID, p.StandardNodeID)
	case KindMove:
		err = profitCenters.touch(ctx, tx, c.ID, "segment_id = $3, ", p.Segment.ID)
	default:
		err = profitCenters.touch(ctx, tx, c.ID, "")
	}
	if err != nil {
		return err
	}
	return recordProfitCenter(ctx, tx, c.ID, ch.Kind, &ch.ID)
}

func writeProfitCenter(ctx context.Context, tx pgx.Tx, c ProfitCenter, ch CenterChange, p profitCenterPayload, spans []span[ProfitCenterAttrs]) error {
	retire, insert, _ := diffSpans(c.Periods, spans)
	if err := retirePeriods(ctx, tx, profitCenters, retire, ch.ID); err != nil {
		return err
	}
	for _, s := range insert {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, valid_to, locked, change_id, created_by, updated_by)
			SELECT tenant_id, $2, id, $3, $4, $5, $6, $7, $7 FROM ledger.profit_center WHERE id = $1`,
			c.ID, uuid.Must(uuid.NewV7()), s.From, nullDate(s.To), s.Attr.Locked, ch.ID, actor(ctx)); err != nil {
			return fmt.Errorf("ledger: insert period of profit center %s from %s: %w", c.ID, dateText(s.From), err)
		}
	}
	if added := newCompanies(c, p.LegalEntityIDs); len(added) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id, created_by)
			SELECT tenant_id, id, le, $3 FROM ledger.profit_center, unnest($2::uuid[]) AS le WHERE id = $1`, c.ID, added, actor(ctx)); err != nil {
			return fmt.Errorf("ledger: assign profit center %s to legal entities %v: %w", c.ID, added, err)
		}
	}
	return nil
}

// closedProfitCenter ends the profit center's pending change without
// applying it: a create leaves the center rejected.
func closedProfitCenter(ctx context.Context, tx pgx.Tx, ch CenterChange) error {
	if ch.Kind != KindCreate {
		return profitCenters.touch(ctx, tx, *ch.ProfitCenterID, "")
	}
	if err := profitCenters.touch(ctx, tx, *ch.ProfitCenterID, "status = 'rejected', "); err != nil {
		return err
	}
	return recordProfitCenter(ctx, tx, *ch.ProfitCenterID, "reject", &ch.ID)
}

// UpdateProfitCenterTexts changes a profit center's texts at once.
func UpdateProfitCenterTexts(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch TextChange) (ProfitCenter, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ProfitCenter{}, err
	}
	c, err := getProfitCenter(ctx, tx, id, forUpdate)
	if err != nil {
		return ProfitCenter{}, err
	}
	if err := updateTexts(ctx, tx, profitCenters, c.centerHead, ifMatch, ch); err != nil {
		return ProfitCenter{}, err
	}
	if err := recordProfitCenter(ctx, tx, id, "texts", nil, ch.fields()...); err != nil {
		return ProfitCenter{}, err
	}
	return getProfitCenter(ctx, tx, id, noLock)
}

var profitCenterEvents = map[string]string{KindCreate: "created", KindRedate: "redated", KindLock: "locked", KindMove: "moved", KindAssign: "assigned",
	"reject": "rejected", "texts": "texts"}

// recordProfitCenter audits a change of the profit center and publishes
// ProfitCenterChanged with the center as it now is.
func recordProfitCenter(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string, change *uuid.UUID, fields ...string) error {
	c, err := getProfitCenter(ctx, tx, id, noLock)
	if err != nil {
		return err
	}
	from, to, _ := validity(c.Periods)
	periods := make([]map[string]any, len(c.Periods))
	for i, s := range c.Periods {
		periods[i] = map[string]any{"valid_from": dateText(s.From), "valid_to": datePtr(s.To), "locked": s.Attr.Locked}
	}
	payload := map[string]any{"profit_center_id": c.ID, "code": c.Code, "name": c.Texts.Name, "status": c.Status, "segment_id": c.SegmentID,
		"legal_entity_ids": c.LegalEntityIDs, "change": profitCenterEvents[action], "center_change_id": change, "valid_from": datePtr(from),
		"valid_to": datePtr(to), "periods": periods}
	detail := map[string]any{"center_change_id": change, "segment_id": c.SegmentID, "legal_entity_ids": c.LegalEntityIDs, "periods": periods}
	if len(fields) > 0 {
		detail["fields"] = fields
	}
	return audit.Record(ctx, "ledger.profit_center."+action, "ledger.profit_center", id, detail,
		outbox.Event{Context: "ledger", Name: "profit_center_changed", AggregateType: "ledger.profit_center", AggregateID: id, Payload: payload})
}
