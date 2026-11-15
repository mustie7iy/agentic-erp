package grc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The statuses of a risk.
const (
	Open   = "open"
	Closed = "closed"
)

// Risk is a row of grc.risk with its latest rating's rater and time and
// the controls that mitigate it now (ADR-0078 §10-§12).
type Risk struct {
	ID         uuid.UUID
	Title      string
	Category   string
	OwnerID    uuid.UUID
	Likelihood int16
	Impact     int16
	Score      int16
	RatedAt    time.Time
	RatedBy    uuid.UUID
	Status     string
	// CloseReason, ClosedAt and ClosedBy are set once the risk is closed.
	CloseReason *string
	ClosedAt    *time.Time
	ClosedBy    *uuid.UUID
	// Controls are the controls that mitigate the risk now, by code;
	// never nil.
	Controls  []RiskControl
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

// RiskControl is a control that mitigates a risk, with its code and its
// status now.
type RiskControl struct {
	ControlID uuid.UUID
	Code      string
	Status    string
}

// ids are the ids of the controls, in their order.
func ids(controls []RiskControl) []uuid.UUID {
	out := make([]uuid.UUID, len(controls))
	for i, c := range controls {
		out[i] = c.ControlID
	}
	return out
}

// Rating is a row of grc.risk_rating.
type Rating struct {
	ID         uuid.UUID
	RiskID     uuid.UUID
	Likelihood int16
	Impact     int16
	RatedBy    uuid.UUID
	RatedAt    time.Time
}

// Score is the rating's likelihood times its impact.
func (r Rating) Score() int16 { return r.Likelihood * r.Impact }

// riskSelect reads risks under the alias r, each with its latest rating's
// time and rater (risk_rating_latest reads it first).
const riskSelect = `SELECT r.id, r.title, r.category, r.owner_id, r.likelihood, r.impact, r.score, rt.created_at, rt.rated_by, r.status,
		r.close_reason, r.closed_at, r.closed_by, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM grc.risk r CROSS JOIN LATERAL (
		SELECT x.created_at, x.rated_by FROM grc.risk_rating x WHERE x.tenant_id = r.tenant_id AND x.risk_id = r.id
		ORDER BY x.id DESC LIMIT 1) rt`

func scanRisk(row pgx.Row) (Risk, error) {
	var r Risk
	err := row.Scan(&r.ID, &r.Title, &r.Category, &r.OwnerID, &r.Likelihood, &r.Impact, &r.Score, &r.RatedAt, &r.RatedBy, &r.Status,
		&r.CloseReason, &r.ClosedAt, &r.ClosedBy, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	r.Controls = []RiskControl{}
	return r, errs.Wrapf(err, "scan risk")
}

// mitigatedBy gives the risks the controls that mitigate them now, each
// risk's by code.
func mitigatedBy(ctx context.Context, tx pgx.Tx, risks []Risk) error {
	byID := make(map[uuid.UUID]*Risk, len(risks))
	keys := make([]uuid.UUID, len(risks))
	for i := range risks {
		byID[risks[i].ID], keys[i] = &risks[i], risks[i].ID
	}
	rows, _ := tx.Query(ctx, `SELECT rc.risk_id, c.id, c.code, c.status FROM grc.risk_control rc
		JOIN grc.control c ON c.tenant_id = rc.tenant_id AND c.id = rc.control_id
		WHERE rc.risk_id = ANY($1) AND rc.status = 'active' ORDER BY rc.risk_id, c.code, c.id`, keys)
	var risk uuid.UUID
	var c RiskControl
	if _, err := pgx.ForEachRow(rows, []any{&risk, &c.ControlID, &c.Code, &c.Status}, func() error {
		byID[risk].Controls = append(byID[risk].Controls, c)
		return nil
	}); err != nil {
		return fmt.Errorf("grc: controls of %d risks: %w", len(risks), err)
	}
	return nil
}

// risk reads a risk of the tenant with its controls, locked for a change
// when forUpdate: a rating locks it too, so ratings, changes and the
// closing of one risk follow one another.
func risk(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Risk, error) {
	q := riskSelect + ` WHERE r.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRisk(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Risk{}, problem.NotFoundOf("risk")
	case err != nil:
		return Risk{}, fmt.Errorf("grc: read risk %s: %w", id, err)
	}
	one := []Risk{r}
	if err := mitigatedBy(ctx, tx, one); err != nil {
		return Risk{}, err
	}
	return one[0], nil
}

// GetRisk returns a risk of the tenant with its controls.
func GetRisk(ctx context.Context, id uuid.UUID) (Risk, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Risk{}, err
	}
	return risk(ctx, tx, id, false)
}

// errClosed refuses a change, a rating and a closing of a closed risk.
func errClosed() *problem.Problem {
	return problem.Conflict("grc.risk_closed", "The risk is closed: it changes no more and takes no rating.")
}

// trimmed checks a text a risk keeps: not white space alone, and without
// white space around it, since the register is listed by its title and
// filtered by its category.
func trimmed(field, value string) error {
	if err := problem.RequireText(field, value); err != nil {
		return err
	}
	if value != strings.TrimSpace(value) {
		return problem.Invalid("grc.invalid_text", field, "The "+field+" is given without leading or trailing white space.")
	}
	return nil
}

// mitigating checks the controls a risk adds (field names the list in a
// refusal): each named once, a control of the library and active. They
// are read FOR SHARE, in the order of their ids, so a control retired
// meanwhile is either seen as retired or retired only after the risk
// names it; a retirement locks one control and no risk, so the two never
// wait for each other in a cycle.
func mitigating(ctx context.Context, tx pgx.Tx, field string, add []uuid.UUID) error {
	for i, id := range add {
		if slices.Index(add, id) < i {
			return problem.Invalid("grc.invalid_control", fmt.Sprintf("%s[%d]", field, i), "A control is named once.")
		}
	}
	rows, _ := tx.Query(ctx, `SELECT id, code, status FROM grc.control WHERE id = ANY($1) ORDER BY id FOR SHARE`, add)
	found := map[uuid.UUID]RiskControl{}
	var c RiskControl
	if _, err := pgx.ForEachRow(rows, []any{&c.ControlID, &c.Code, &c.Status}, func() error { found[c.ControlID] = c; return nil }); err != nil {
		return fmt.Errorf("grc: controls of a risk: %w", err)
	}
	for i, id := range add {
		at := fmt.Sprintf("%s[%d]", field, i)
		switch c, ok := found[id]; {
		case !ok:
			return problem.ReferenceNotFound(at, "control", "no control with this id")
		case c.Status != Active:
			return problem.Invalid("grc.invalid_control", at, "Control "+c.Code+" is retired: a risk is mitigated by active controls.")
		}
	}
	return nil
}

// name adds the controls to a risk, by the caller p.
func name(ctx context.Context, tx pgx.Tx, p reqctx.Principal, risk uuid.UUID, controls []uuid.UUID) error {
	for _, c := range controls {
		if _, err := tx.Exec(ctx, `INSERT INTO grc.risk_control (tenant_id, id, risk_id, control_id, created_by) VALUES ($1, $2, $3, $4, $5)`,
			p.TenantID, uuid.Must(uuid.NewV7()), risk, c, p.ID); err != nil {
			return fmt.Errorf("grc: control %s of risk %s: %w", c, risk, err)
		}
	}
	return nil
}

// rate records a rating of a risk by the caller p.
func rate(ctx context.Context, tx pgx.Tx, p reqctx.Principal, risk uuid.UUID, likelihood, impact int16) (Rating, error) {
	r := Rating{ID: uuid.Must(uuid.NewV7()), RiskID: risk, Likelihood: likelihood, Impact: impact, RatedBy: p.ID}
	if err := tx.QueryRow(ctx, `INSERT INTO grc.risk_rating (tenant_id, id, risk_id, likelihood, impact, rated_by) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`, p.TenantID, r.ID, risk, likelihood, impact, p.ID).Scan(&r.RatedAt); err != nil {
		return Rating{}, fmt.Errorf("grc: rate risk %s: %w", risk, err)
	}
	return r, nil
}

// riskChange is one change of a risk as it is recorded: the last word of
// its audit action, what the event calls it, the audit detail, and the
// rating it made, nil for a change without one.
type riskChange struct {
	action, name string
	detail       map[string]any
	rating       *Rating
}

// riskChanged adds the audit record of a risk's change and publishes
// RiskChanged in the same transaction (ADR-0007, ADR-0011; ADR-0078
// §12). Neither carries a closing's reason: free text of a person's,
// which may name people.
func riskChanged(ctx context.Context, r Risk, ch riskChange) error {
	var rating any
	if x := ch.rating; x != nil {
		rating = map[string]any{"rating_id": x.ID, "likelihood": x.Likelihood, "impact": x.Impact, "score": x.Score(), "rated_by": x.RatedBy}
	}
	payload := map[string]any{"risk_id": r.ID, "title": r.Title, "category": r.Category, "owner_id": r.OwnerID, "likelihood": r.Likelihood,
		"impact": r.Impact, "score": r.Score, "control_ids": ids(r.Controls), "status": r.Status, "change": ch.name, "rating": rating, "version": r.Version}
	if err := audit.Record(ctx, "grc.risk."+ch.action, "grc.risk", r.ID, ch.detail,
		outbox.Event{Context: "grc", Name: "risk_changed", AggregateType: "grc.risk", AggregateID: r.ID, Payload: payload}); err != nil {
		return fmt.Errorf("grc: record %s of risk %s: %w", ch.action, r.ID, err)
	}
	return nil
}
