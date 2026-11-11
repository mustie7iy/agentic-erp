package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// costCenterPayload is what a cost center change asks for; each kind uses
// its fields (dates as YYYY-MM-DD, "" open-ended or, for a lock, to the
// end of validity).
type costCenterPayload struct {
	ValidFrom      string     `json:"valid_from,omitempty"`
	ValidTo        string     `json:"valid_to,omitempty"`
	Category       string     `json:"category,omitempty"`
	ProfitCenterID *uuid.UUID `json:"profit_center_id,omitempty"` // create
	StandardNodeID *uuid.UUID `json:"standard_node_id,omitempty"` // create: its node in the standard hierarchy (T56b)
	LockCost       *bool      `json:"lock_actual_cost,omitempty"`
	LockRevenue    *bool      `json:"lock_actual_revenue,omitempty"`
	LockPlan       *bool      `json:"lock_plan,omitempty"`
	// ProfitCenter is a move's new profit center: absent unchanged, its id
	// null for none.
	ProfitCenter *pcChoice `json:"profit_center,omitempty"`
}

type pcChoice struct {
	ID *uuid.UUID `json:"id"`
}

// NewCostCenter is a cost center to create.
type NewCostCenter struct {
	LegalEntityID  uuid.UUID
	Code           string
	Texts          Texts
	Category       string
	ProfitCenterID *uuid.UUID
	StandardNodeID *uuid.UUID // its node in the standard hierarchy; nil: the UNASSIGNED leaf, at approval (unassignedLeaf)
	ValidFrom      time.Time
	ValidTo        time.Time // zero: open-ended
	Reason         string
}

func invalidValidity(field, message string) error {
	return problem.Unprocessable("ledger.invalid_validity", message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

func nothingToChange() error {
	return problem.Unprocessable("ledger.nothing_to_change", "The change would leave the center as it is.")
}

// checkDates refuses a validity that ends before it starts.
func checkDates(from, to time.Time) error {
	if !to.IsZero() && !to.After(from) {
		return invalidValidity("valid_to", "valid_to is exclusive and must be after valid_from")
	}
	return nil
}

// RequestCostCenter creates a pending cost center and its create change,
// with its approval request.
func (e *Endpoints) RequestCostCenter(ctx context.Context, in NewCostCenter) (CostCenter, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CostCenter{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return CostCenter{}, err
	}
	in.ValidFrom, in.ValidTo = day(in.ValidFrom), dayOrOpen(in.ValidTo)
	if err := checkDates(in.ValidFrom, in.ValidTo); err != nil {
		return CostCenter{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, description, responsible, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $8)`, p.TenantID, id, le.ID, in.Code, in.Texts.Name, in.Texts.Description, in.Texts.Responsible, p.ID); err != nil {
		return CostCenter{}, fmt.Errorf("ledger: insert cost center %s: %w", in.Code, err)
	}
	if in.StandardNodeID != nil {
		if err := checkStandardNode(ctx, tx, HierarchyCostCenters, id, *in.StandardNodeID); err != nil {
			return CostCenter{}, err
		}
	}
	payload := costCenterPayload{ValidFrom: dateText(in.ValidFrom), ValidTo: dateText(in.ValidTo), Category: in.Category, ProfitCenterID: in.ProfitCenterID,
		StandardNodeID: in.StandardNodeID}
	c := CostCenter{LegalEntityID: le.ID}
	if _, err := planCostCenter(ctx, tx, c, KindCreate, payload, e.days()); err != nil {
		return CostCenter{}, err
	}
	if _, err := requestChange(ctx, tx, e.Approvals, costCenterRef(le, id, in.Code), KindCreate, payload, in.Reason); err != nil {
		return CostCenter{}, err
	}
	return getCostCenter(ctx, tx, authz.Access{All: true}, id, false)
}

func costCenterRef(le tenancy.Entity, id uuid.UUID, code string) centerRef {
	return centerRef{kind: costCenters, id: id, title: fmt.Sprintf("Cost center %s/%s", le.Code, code)}
}

// dayOrOpen is day(t), or zero for an open end; the last day spans can
// hold (forever) is an open end too, as spans read it.
func dayOrOpen(t time.Time) time.Time {
	if t.IsZero() || !day(t).Before(forever) {
		return time.Time{}
	}
	return day(t)
}

// RequestCostCenterChange opens a change of an active cost center: a new
// validity, locks, or a move to another profit center or category. The
// payload is built from the center as it is (a new validity keeps the
// date it does not change).
func (e *Endpoints) RequestCostCenterChange(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, kind string,
	build func(from, to time.Time) costCenterPayload, reason string) (CenterChange, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CenterChange{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return CenterChange{}, err
	}
	c, err := getCostCenter(ctx, tx, access, id, true)
	if err != nil {
		return CenterChange{}, err
	}
	if err := changeable(c.centerHead, ifMatch); err != nil {
		return CenterChange{}, err
	}
	from, to, _ := validity(c.Periods)
	payload := build(from, to)
	if _, err := planCostCenter(ctx, tx, c, kind, payload, e.days()); err != nil {
		return CenterChange{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID)
	if err != nil {
		return CenterChange{}, fmt.Errorf("ledger: legal entity of cost center %s: %w", id, err)
	}
	return requestChange(ctx, tx, e.Approvals, costCenterRef(le, id, c.Code), kind, payload, reason)
}

// changeable checks that a center may be changed now: the version the
// caller saw, and active (a pending one is being created).
func changeable(h centerHead, ifMatch func(int32) error) error {
	if err := ifMatch(h.Version); err != nil {
		return err
	}
	if h.Status != CenterActive {
		return problem.Conflict("ledger.invalid_center_status", "The center is "+h.Status+"; this requires status active.")
	}
	return nil
}

// planCostCenter computes the cost center's periods after the change and
// checks them as of its company's date (days); it refuses a change that
// changes nothing.
func planCostCenter(ctx context.Context, tx pgx.Tx, c CostCenter, kind string, p costCenterPayload, days companyDays) ([]span[CostCenterAttrs], error) {
	from, to := parseDate(p.ValidFrom), parseDate(p.ValidTo)
	cat, err := costCenterCategory(ctx, tx, p.Category)
	if err != nil {
		return nil, err
	}
	var spans []span[CostCenterAttrs]
	switch kind {
	case KindCreate:
		if p.ProfitCenterID != nil {
			cat.ProfitCenterID = *p.ProfitCenterID
		}
		spans = []span[CostCenterAttrs]{{From: from, To: to, Attr: cat}}
	case KindRedate:
		if err := checkDates(from, to); err != nil {
			return nil, err
		}
		var ok bool
		if spans, ok = redated(c.Periods, from, to); !ok {
			return nil, invalidValidity("valid_from", "the new validity does not overlap the current one")
		}
	case KindLock:
		if to.IsZero() {
			_, to, _ = validity(c.Periods)
		}
		if err := checkDates(from, to); err != nil {
			return nil, err
		}
		if !within(c.Periods, from, to) {
			return nil, invalidValidity("valid_from", "the locks must lie within the cost center's validity")
		}
		spans = setFrom(c.Periods, from, to, func(a CostCenterAttrs) CostCenterAttrs {
			a.LockCost, a.LockRevenue, a.LockPlan = flag(p.LockCost, a.LockCost), flag(p.LockRevenue, a.LockRevenue), flag(p.LockPlan, a.LockPlan)
			return a
		})
	default: // move
		if !within(c.Periods, from, from.AddDate(0, 0, 1)) {
			return nil, invalidValidity("valid_from", "the move must start within the cost center's validity")
		}
		spans = setFrom(c.Periods, from, time.Time{}, func(a CostCenterAttrs) CostCenterAttrs { return moved(a, p) })
	}
	return spans, checkCostCenterSpans(ctx, tx, c, kind, spans, days)
}

// checkCostCenterSpans checks the cost center's periods after a change:
// that they change something, that its posted lines do not contradict
// them (costCenterInUse), that a default cost center stays usable
// (keepsDefaultCostCenter) and their profit centers.
func checkCostCenterSpans(ctx context.Context, tx pgx.Tx, c CostCenter, kind string, spans []span[CostCenterAttrs], days companyDays) error {
	if _, _, differ := diffSpans(c.Periods, spans); !differ {
		return nothingToChange()
	}
	if kind != KindCreate {
		if err := costCenterInUse(ctx, tx, c, spans); err != nil {
			return err
		}
	}
	if kind == KindRedate || kind == KindLock {
		if err := keepsDefaultCostCenter(ctx, tx, c, spans, days); err != nil {
			return err
		}
	}
	return checkProfitCenters(ctx, tx, c.LegalEntityID, spans)
}

// costCenterCategory reads the category a change gives, an active one of
// the configuration (controlling.cost_center_categories, T56c), as a new
// cost center's attributes: the category and the locks it starts with; a
// change that gives none has none. The category is read for share, so an
// activation deactivating it waits for this transaction (422
// ledger.cost_center_category_invalid otherwise).
func costCenterCategory(ctx context.Context, tx pgx.Tx, code string) (CostCenterAttrs, error) {
	a := CostCenterAttrs{Category: code}
	if code == "" {
		return a, nil
	}
	err := tx.QueryRow(ctx, `SELECT lock_actual_cost, lock_actual_revenue, lock_plan FROM ledger.cost_center_category
		WHERE code = $1 AND is_active FOR SHARE`, code).Scan(&a.LockCost, &a.LockRevenue, &a.LockPlan)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, problem.Unprocessable("ledger.cost_center_category_invalid", "Category "+code+" is not one of the configuration's cost center categories.",
			problem.FieldError{Field: "category", Code: "unknown", Message: "a category of controlling.cost_center_categories"})
	}
	return a, errs.Wrapf(err, "ledger: cost center category %s", code)
}

func flag(set *bool, current bool) bool {
	if set == nil {
		return current
	}
	return *set
}

func moved(a CostCenterAttrs, p costCenterPayload) CostCenterAttrs {
	if p.Category != "" {
		a.Category = p.Category
	}
	if p.ProfitCenter != nil {
		a.ProfitCenterID = uuid.Nil
		if p.ProfitCenter.ID != nil {
			a.ProfitCenterID = *p.ProfitCenter.ID
		}
	}
	return a
}

// checkProfitCenters checks the profit center of each period to be
// written: active, assigned to the cost center's legal entity and valid
// over the period. Each is read for share, so its validity does not
// change before this transaction ends (redating it takes it for update).
func checkProfitCenters(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, spans []span[CostCenterAttrs]) error {
	for _, s := range spans {
		if s.ID != uuid.Nil || s.Attr.ProfitCenterID == uuid.Nil {
			continue
		}
		pc, err := getProfitCenter(ctx, tx, s.Attr.ProfitCenterID, forShare)
		reason := ""
		switch {
		case problem.From(err).Code == "request.not_found":
			reason = "unknown"
		case err != nil:
			return err
		case pc.Status != CenterActive:
			reason = "not_active"
		case !slices.Contains(pc.LegalEntityIDs, legalEntity):
			reason = "not_in_company"
		case !within(pc.Periods, s.From, s.To):
			reason = "not_valid_on_date"
		}
		if reason != "" {
			return problem.Unprocessable("ledger.profit_center_invalid", "The profit center cannot be the cost center's from "+dateText(s.From)+": "+reason+".",
				problem.FieldError{Field: "profit_center_id", Code: reason, Message: "an active profit center of the legal entity, valid over the period"})
		}
	}
	return nil
}

// applyCostCenterChange applies an approved change of a cost center, as of
// its company's date (days).
func applyCostCenterChange(ctx context.Context, tx pgx.Tx, ch CenterChange, days companyDays) error {
	c, err := getCostCenter(ctx, tx, authz.Access{All: true}, *ch.CostCenterID, true)
	if err != nil {
		return err
	}
	var p costCenterPayload
	decode(ch.Payload, &p)
	spans, err := planCostCenter(ctx, tx, c, ch.Kind, p, days)
	if err != nil {
		return err
	}
	retire, insert, _ := diffSpans(c.Periods, spans)
	if err := writeCostCenterPeriods(ctx, tx, c, ch.ID, retire, insert); err != nil {
		return err
	}
	if ch.Kind != KindCreate {
		if err := costCenters.touch(ctx, tx, c.ID, ""); err != nil {
			return err
		}
	} else if err := activate(ctx, tx, costCenters, c.ID, ch.ID, p.StandardNodeID); err != nil {
		return err
	}
	return recordCostCenter(ctx, tx, c.ID, ch.Kind, &ch.ID)
}

func writeCostCenterPeriods(ctx context.Context, tx pgx.Tx, c CostCenter, change uuid.UUID, retire []uuid.UUID, insert []span[CostCenterAttrs]) error {
	if err := retirePeriods(ctx, tx, costCenters, retire, change); err != nil {
		return err
	}
	for _, s := range insert {
		var pc *uuid.UUID
		if s.Attr.ProfitCenterID != uuid.Nil {
			pc = &s.Attr.ProfitCenterID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, valid_to, category,
				profit_center_id, lock_actual_cost, lock_actual_revenue, lock_plan, change_id, created_by, updated_by)
			SELECT tenant_id, $2, id, legal_entity_id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11 FROM ledger.cost_center WHERE id = $1`,
			c.ID, uuid.Must(uuid.NewV7()), s.From, nullDate(s.To), s.Attr.Category, pc, s.Attr.LockCost, s.Attr.LockRevenue, s.Attr.LockPlan,
			change, actor(ctx)); err != nil {
			return fmt.Errorf("ledger: insert period of cost center %s from %s: %w", c.ID, dateText(s.From), err)
		}
	}
	return nil
}

// retirePeriods retires the periods a change replaced.
func retirePeriods(ctx context.Context, tx pgx.Tx, k centerKind, ids []uuid.UUID, change uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE `+k.ident(k.periods)+` SET retired_by_change_id = $2, version = version + 1, updated_by = $3 WHERE id = ANY($1)`,
		ids, change, actor(ctx))
	return errs.Wrapf(err, "ledger: retire %s periods replaced by center change %s", k.table, change)
}

// nullDate is an open end as the database holds it.
func nullDate(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// closedCostCenter ends the cost center's pending change without applying
// it: a create leaves the center rejected.
func closedCostCenter(ctx context.Context, tx pgx.Tx, ch CenterChange) error {
	if ch.Kind != KindCreate {
		return costCenters.touch(ctx, tx, *ch.CostCenterID, "")
	}
	if err := costCenters.touch(ctx, tx, *ch.CostCenterID, "status = 'rejected', "); err != nil {
		return err
	}
	return recordCostCenter(ctx, tx, *ch.CostCenterID, "reject", &ch.ID)
}

// UpdateCostCenterTexts changes a cost center's texts at once.
func UpdateCostCenterTexts(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch TextChange) (CostCenter, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CostCenter{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return CostCenter{}, err
	}
	c, err := getCostCenter(ctx, tx, access, id, true)
	if err != nil {
		return CostCenter{}, err
	}
	if err := updateTexts(ctx, tx, costCenters, c.centerHead, ifMatch, ch); err != nil {
		return CostCenter{}, err
	}
	if err := recordCostCenter(ctx, tx, id, "texts", nil, ch.fields()...); err != nil {
		return CostCenter{}, err
	}
	return getCostCenter(ctx, tx, access, id, false)
}

// costCenterEvents name what happened to a cost center, by the audit
// action that records it.
var costCenterEvents = map[string]string{KindCreate: "created", KindRedate: "redated", KindLock: "locked", KindMove: "moved", "reject": "rejected",
	"texts": "texts"}

// recordCostCenter audits a change of the cost center and publishes
// CostCenterChanged with the center as it now is.
func recordCostCenter(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string, change *uuid.UUID, fields ...string) error {
	c, err := getCostCenter(ctx, tx, authz.Access{All: true}, id, false)
	if err != nil {
		return err
	}
	from, to, _ := validity(c.Periods)
	periods := make([]map[string]any, len(c.Periods))
	for i, s := range c.Periods {
		var pc *uuid.UUID
		if s.Attr.ProfitCenterID != uuid.Nil {
			pc = &s.Attr.ProfitCenterID
		}
		periods[i] = map[string]any{"valid_from": dateText(s.From), "valid_to": datePtr(s.To), "category": s.Attr.Category, "profit_center_id": pc,
			"lock_actual_cost": s.Attr.LockCost, "lock_actual_revenue": s.Attr.LockRevenue, "lock_plan": s.Attr.LockPlan}
	}
	payload := map[string]any{"cost_center_id": c.ID, "legal_entity_id": c.LegalEntityID, "code": c.Code, "name": c.Texts.Name, "status": c.Status,
		"change": costCenterEvents[action], "center_change_id": change, "valid_from": datePtr(from), "valid_to": datePtr(to), "periods": periods}
	detail := map[string]any{"center_change_id": change, "periods": periods}
	if len(fields) > 0 {
		detail["fields"] = fields
	}
	return audit.Record(ctx, "ledger.cost_center."+action, "ledger.cost_center", id, detail,
		outbox.Event{Context: "ledger", Name: "cost_center_changed", AggregateType: "ledger.cost_center", AggregateID: id, Payload: payload})
}
