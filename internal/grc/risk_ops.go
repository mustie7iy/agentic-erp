package grc

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// NewRisk is a risk as a client records it, with its first rating.
type NewRisk struct {
	Title      string
	Category   string
	OwnerID    uuid.UUID
	Likelihood int16
	Impact     int16
	// ControlIDs are the active controls that mitigate it, each once; nil
	// for none.
	ControlIDs []uuid.UUID
}

// riskTexts checks a risk's title and category.
func riskTexts(title, category string) error {
	if err := trimmed("title", title); err != nil {
		return err
	}
	return trimmed("category", category)
}

// RecordRisk records a risk in the tenant's register (ADR-0078 §10-§12,
// T98c), open, with its first rating by the caller and the controls that
// mitigate it, its audit record and RiskChanged.
func (e *Endpoints) RecordRisk(ctx context.Context, in NewRisk) (Risk, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Risk{}, err
	}
	if err := riskTexts(in.Title, in.Category); err != nil {
		return Risk{}, err
	}
	if err := e.owner(ctx, in.OwnerID, "risk"); err != nil {
		return Risk{}, err
	}
	if err := mitigating(ctx, tx, "control_ids", in.ControlIDs); err != nil {
		return Risk{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO grc.risk (tenant_id, id, title, category, owner_id, likelihood, impact, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, p.TenantID, id, in.Title, in.Category, in.OwnerID, in.Likelihood, in.Impact, p.ID); err != nil {
		return Risk{}, fmt.Errorf("grc: record risk %q: %w", in.Title, err)
	}
	rating, err := rate(ctx, tx, p, id, in.Likelihood, in.Impact)
	if err != nil {
		return Risk{}, err
	}
	if err := name(ctx, tx, p, id, in.ControlIDs); err != nil {
		return Risk{}, err
	}
	r, err := risk(ctx, tx, id, false)
	if err != nil {
		return Risk{}, err
	}
	return r, riskChanged(ctx, r, riskChange{action: "create", name: "created", rating: &rating, detail: map[string]any{"title": r.Title,
		"category": r.Category, "owner_id": r.OwnerID, "likelihood": r.Likelihood, "impact": r.Impact, "control_ids": ids(r.Controls)}})
}

// RiskChange is a merge patch of a risk: nil fields stay.
type RiskChange struct {
	Title    *string
	Category *string
	OwnerID  *uuid.UUID
	// ControlIDs replaces the risk's controls as a whole; an empty list
	// leaves it without any.
	ControlIDs *[]uuid.UUID
}

// riskEdit is what a change does to a risk: its texts and owner after
// it, the controls it adds and removes, and the names of the fields it
// changes, in the contract's order.
type riskEdit struct {
	title, category string
	owner           uuid.UUID
	add, remove     []uuid.UUID
	fields          []string
}

// apply is the edit the change makes to the risk.
func (ch RiskChange) apply(r Risk) riskEdit {
	ed := riskEdit{title: r.Title, category: r.Category, owner: r.OwnerID}
	if ch.Title != nil && *ch.Title != r.Title {
		ed.title, ed.fields = *ch.Title, append(ed.fields, "title")
	}
	if ch.Category != nil && *ch.Category != r.Category {
		ed.category, ed.fields = *ch.Category, append(ed.fields, "category")
	}
	if ch.OwnerID != nil && *ch.OwnerID != r.OwnerID {
		ed.owner, ed.fields = *ch.OwnerID, append(ed.fields, "owner_id")
	}
	if ch.ControlIDs != nil {
		now := ids(r.Controls)
		for _, c := range *ch.ControlIDs {
			if !slices.Contains(now, c) {
				ed.add = append(ed.add, c)
			}
		}
		for _, c := range now {
			if !slices.Contains(*ch.ControlIDs, c) {
				ed.remove = append(ed.remove, c)
			}
		}
		if len(ed.add)+len(ed.remove) > 0 {
			ed.fields = append(ed.fields, "control_ids")
		}
	}
	return ed
}

// check refuses an edit that changes nothing or says what a risk may
// not: its texts, a new owner, the controls it adds (each also checked
// against the list given, so one named twice is refused).
func (e *Endpoints) check(ctx context.Context, tx pgx.Tx, ch RiskChange, ed riskEdit, r Risk) error {
	if len(ed.fields) == 0 {
		return problem.Unprocessable("grc.nothing_to_change", "The change would leave the risk as it is.")
	}
	if err := riskTexts(ed.title, ed.category); err != nil {
		return err
	}
	if ed.owner != r.OwnerID {
		if err := e.owner(ctx, ed.owner, "risk"); err != nil {
			return err
		}
	}
	if ch.ControlIDs == nil {
		return nil
	}
	given := *ch.ControlIDs
	for i, c := range given {
		if slices.Index(given, c) < i {
			return problem.Invalid("grc.invalid_control", fmt.Sprintf("control_ids[%d]", i), "A control is named once.")
		}
	}
	return mitigating(ctx, tx, "control_ids", ed.add)
}

// ChangeRisk changes what an open risk says, with the caller's version of
// it (ADR-0008, ADR-0078 §10): the row is locked, the version compared
// and the change recorded with the fields it changed. Controls left out
// are kept as removed; the rating is Rate's.
func (e *Endpoints) ChangeRisk(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch RiskChange) (Risk, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Risk{}, err
	}
	r, err := risk(ctx, tx, id, true)
	if err != nil {
		return Risk{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Risk{}, err
	}
	if r.Status != Open {
		return Risk{}, errClosed()
	}
	ed := ch.apply(r)
	if err := e.check(ctx, tx, ch, ed, r); err != nil {
		return Risk{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.risk SET title = $2, category = $3, owner_id = $4, version = version + 1, updated_by = $5
		WHERE id = $1 AND status = 'open'`, id, ed.title, ed.category, ed.owner, p.ID); err != nil {
		return Risk{}, fmt.Errorf("grc: change risk %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.risk_control SET status = 'removed', removed_at = now(), removed_by = $3
		WHERE risk_id = $1 AND control_id = ANY($2) AND status = 'active'`, id, ed.remove, p.ID); err != nil {
		return Risk{}, fmt.Errorf("grc: remove controls of risk %s: %w", id, err)
	}
	if err := name(ctx, tx, p, id, ed.add); err != nil {
		return Risk{}, err
	}
	after, err := risk(ctx, tx, id, false)
	if err != nil {
		return Risk{}, err
	}
	return after, riskChanged(ctx, after, riskChange{action: "change", name: "changed", detail: map[string]any{"fields": ed.fields,
		"controls_added": nonNil(ed.add), "controls_removed": nonNil(ed.remove)}})
}

// nonNil is the list, empty rather than nil, so a record says "none".
func nonNil(l []uuid.UUID) []uuid.UUID {
	if l == nil {
		return []uuid.UUID{}
	}
	return l
}

// Rate rates an open risk again by the caller (ADR-0078 §11): the rating
// is kept, and its likelihood and impact become the risk's, at the next
// version. The risk's row is locked, so ratings follow one another and
// never pass a closing.
func Rate(ctx context.Context, id uuid.UUID, likelihood, impact int16) (Rating, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Rating{}, err
	}
	r, err := risk(ctx, tx, id, true)
	if err != nil {
		return Rating{}, err
	}
	if r.Status != Open {
		return Rating{}, errClosed()
	}
	rating, err := rate(ctx, tx, p, id, likelihood, impact)
	if err != nil {
		return Rating{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.risk SET likelihood = $2, impact = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND status = 'open'`, id, likelihood, impact, p.ID); err != nil {
		return Rating{}, fmt.Errorf("grc: rating of risk %s: %w", id, err)
	}
	after, err := risk(ctx, tx, id, false)
	if err != nil {
		return Rating{}, err
	}
	return rating, riskChanged(ctx, after, riskChange{action: "rate", name: "rated", rating: &rating, detail: map[string]any{
		"rating_id": rating.ID, "likelihood": likelihood, "impact": impact, "score": rating.Score()}})
}

// CloseRisk closes an open risk for good, once, with the caller's
// version of it and the reason: it changes no more and takes no rating.
func CloseRisk(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Risk, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Risk{}, err
	}
	r, err := risk(ctx, tx, id, true)
	if err != nil {
		return Risk{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Risk{}, err
	}
	if r.Status != Open {
		return Risk{}, errClosed()
	}
	if err := trimmed("reason", reason); err != nil {
		return Risk{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.risk SET status = 'closed', close_reason = $2, closed_at = now(), closed_by = $3, version = version + 1,
		updated_by = $3 WHERE id = $1 AND status = 'open'`, id, reason, p.ID); err != nil {
		return Risk{}, fmt.Errorf("grc: close risk %s: %w", id, err)
	}
	closed, err := risk(ctx, tx, id, false)
	if err != nil {
		return Risk{}, err
	}
	return closed, riskChanged(ctx, closed, riskChange{action: "close", name: "closed", detail: map[string]any{"title": closed.Title}})
}
