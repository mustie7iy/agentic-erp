package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// KindRun is the kind of a consolidation run's Operation (ADR-0059 §6).
const KindRun = "consolidation.run"

// Run statuses (ADR-0059 §5).
const (
	RunQueued     = "queued"
	RunRunning    = "running"
	RunComputed   = "computed"
	RunFailed     = "failed"
	RunDiscarded  = "discarded"
	RunFinal      = "final"
	RunSuperseded = "superseded"
)

func init() {
	// Two runs of a group started at once: the second's insert meets the
	// first's queued row.
	problem.RegisterConstraint("consolidation_run_one_at_a_time", func() *problem.Problem {
		return problem.Conflict("consolidation.run_in_progress", "Another run of the group was started at the same time; it is queued or running "+
			"(GET /v1/consolidation-runs?status=queued).")
	})
	// Finalizing locks the period's runs first (FinalizeConsolidationRun),
	// so a second final of a period is never inserted; should a path ever
	// reach the index, it is a conflict, not an internal error.
	problem.RegisterConstraint("consolidation_run_one_final", func() *problem.Problem {
		return problem.Conflict("consolidation.final_exists", "Another run of the group's period was finalized at the same time; "+
			"read the period's runs (GET /v1/consolidation-runs?status=final) and finalize again to supersede it.")
	})
}

// Run is a stored consolidation run.
type Run struct {
	ID, ParentID, OperationID uuid.UUID
	GroupCode                 string
	Companies                 []uuid.UUID
	FiscalYear, Period        int16
	PeriodStart, PeriodEnd    time.Time
	ConfigVersion             int32
	GroupCurrency, RateType   string
	Status                    string
	Problem                   *RunProblem
	ComputedAt                *time.Time
	FinalizedBy, SupersededBy *uuid.UUID
	FinalizedAt               *time.Time
	Version                   int32
	CreatedAt, UpdatedAt      time.Time
}

// RunProblem is what failed a run: its problem's code and detail.
type RunProblem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// runPath is a run's resource path.
func runPath(id uuid.UUID) string { return "/v1/consolidation-runs/" + id.String() }

// runSelect reads runs.
const runSelect = `SELECT id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start, period_end, config_version, group_currency,
		rate_type, status, problem, operation_id, computed_at, finalized_by, finalized_at, superseded_by, version, created_at, updated_at
	FROM consolidation.run`

func scanRun(r pgx.Row) (Run, error) {
	var x Run
	var failure []byte
	err := r.Scan(&x.ID, &x.GroupCode, &x.ParentID, &x.Companies, &x.FiscalYear, &x.Period, &x.PeriodStart, &x.PeriodEnd, &x.ConfigVersion,
		&x.GroupCurrency, &x.RateType, &x.Status, &failure, &x.OperationID, &x.ComputedAt, &x.FinalizedBy, &x.FinalizedAt, &x.SupersededBy, &x.Version,
		&x.CreatedAt, &x.UpdatedAt)
	if err == nil && failure != nil {
		x.Problem = &RunProblem{}
		// The run wrote it from a RunProblem (FailConsolidationRun); a
		// document that does not decode shows as empty rather than failing
		// the read.
		_ = json.Unmarshal(failure, x.Problem)
	}
	return x, errs.Wrapf(err, "scan consolidation run")
}

// visibleRun reports whether the caller may act in every company of the
// run's group (ADR-0059 §12).
func visibleRun(r Run, access authz.Access) bool {
	return !slices.ContainsFunc(r.Companies, func(id uuid.UUID) bool { return !access.Allows(id) })
}

// readRun reads a run of a group whose companies the caller may all act
// in, FOR UPDATE with lock; any other is not found, as one that does not
// exist, so neither its existence nor its companies leak.
func readRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Run, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	q := runSelect + ` WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanRun(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !visibleRun(r, access)):
		return Run{}, problem.NotFoundOf("consolidation run")
	case err != nil:
		return Run{}, fmt.Errorf("consolidation: run %s: %w", id, err)
	}
	return r, nil
}

// target is what a run of a group and period consolidates (ADR-0059 §6):
// the group as a document has it, its parent's period in the leading
// ledger, and the companies it includes, the parent first and then the
// members whose first period is on or before the run's by company code,
// each with its closing rate.
type target struct {
	doc       *config.Config
	group     config.ConsolidationGroup
	companies map[string]Company
	currency  string
	rateType  string
	year, per int16
	period    Period
	included  []includedCompany
}

// includedCompany is a company a run includes: the parent (member nil) or
// a member, with its first period and closing rate.
type includedCompany struct {
	co                Company
	member            *config.GroupMember
	firstYear, firstP int16
	rate              Rate
}

// all are the ids of the group's parent and every member, included or not.
func (t target) all() []uuid.UUID {
	out := []uuid.UUID{t.companies[t.group.Parent].ID}
	for _, m := range t.group.Members {
		out = append(out, t.companies[m.Company].ID)
	}
	return out
}

// targetOf reads and checks what a run of the group's period consolidates
// under the document: the group visible to the caller (404
// consolidation.group_unknown), the period regular (422
// consolidation.special_period) and the parent's (422
// consolidation.period_unknown), the parent in the group currency (422
// consolidation.parent_currency), the members included on the parent's
// fiscal calendar (422 consolidation.calendar_mismatch), incorporated by
// the parent and held whole (422 consolidation.structure_unsupported, As
// built T68h), each company currency's closing rate there and not stale
// (closingRate), and each member's acquisition measurable (acquirable).
func (e *Endpoints) targetOf(ctx context.Context, tx pgx.Tx, doc *config.Config, code string, year, per int32) (target, error) {
	t := target{doc: doc, rateType: doc.Consolidation.RateTypeOrAverage(), year: int16(year), per: int16(per)}
	g := doc.Consolidation.Group(code)
	var err error
	if t.companies, err = e.companiesByCode(ctx); err != nil {
		return target{}, fmt.Errorf("consolidation: companies: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return target{}, fmt.Errorf("consolidation: company access: %w", err)
	}
	if g == nil || !visible(*g, t.companies, access) {
		return target{}, groupUnknown(code)
	}
	t.group = *g
	if per > 12 {
		return target{}, problem.Unprocessable("consolidation.special_period", fmt.Sprintf("Period %d is a special period; a consolidation run is "+
			"of a regular period (1-12).", per), problem.FieldError{Field: "period", Code: "special_period", Message: "a regular period (1-12)"})
	}
	if t.currency, err = e.Companies.GroupCurrency(ctx); err != nil {
		return target{}, fmt.Errorf("consolidation: group currency: %w", err)
	}
	parent := t.companies[g.Parent]
	if parent.LocalCurrency != t.currency {
		return target{}, problem.Unprocessable("consolidation.parent_currency", fmt.Sprintf("The parent of group %s, company %s, keeps its books in "+
			"%s, not in the group currency %s; a run consolidates a parent in the group currency (ADR-0059 §3).", g.Code, parent.Code,
			parent.LocalCurrency, t.currency), problem.FieldError{Field: "group_code", Code: "parent_currency", Message: "a group whose parent is in " + t.currency})
	}
	p, ok, err := e.Ledger.PeriodOf(ctx, parent.ID, t.year, t.per)
	switch {
	case err != nil:
		return target{}, fmt.Errorf("consolidation: period %d/%d of %s: %w", year, per, parent.Code, err)
	case !ok:
		return target{}, problem.Unprocessable("consolidation.period_unknown", fmt.Sprintf("Company %s, the parent of group %s, has no period %d/%d "+
			"in its leading ledger.", parent.Code, g.Code, year, per), problem.FieldError{Field: "period", Code: "period_unknown",
			Message: "a period of the parent's fiscal calendar"})
	}
	t.period = p
	return t, e.include(ctx, tx, &t)
}

// include lists the companies the run includes with their closing rates.
func (e *Endpoints) include(ctx context.Context, tx pgx.Tx, t *target) error {
	parent := t.companies[t.group.Parent]
	members := slices.Clone(t.group.Members)
	slices.SortFunc(members, func(a, b config.GroupMember) int { return strings.Compare(a.Company, b.Company) })
	t.included = []includedCompany{{co: parent}}
	for i := range members {
		m := &members[i]
		year, per, _ := m.First() // validated: well formed
		if year > int(t.year) || (year == int(t.year) && per > int(t.per)) {
			continue
		}
		co := t.companies[m.Company]
		if co.FiscalCalendarID != parent.FiscalCalendarID {
			return problem.Unprocessable("consolidation.calendar_mismatch", fmt.Sprintf("Company %s, a member of group %s, keeps another fiscal "+
				"calendar than its parent %s; a run consolidates members on the parent's calendar (ADR-0059 §1).", co.Code, t.group.Code, parent.Code),
				problem.FieldError{Field: "group_code", Code: "calendar_mismatch", Message: "company " + co.Code + " on the parent's calendar"})
		}
		if !m.WhollyOwned() || m.Origin != config.OriginIncorporated {
			return structureUnsupported(t.group.Code, *m)
		}
		t.included = append(t.included, includedCompany{co: co, member: m, firstYear: int16(year), firstP: int16(per)})
	}
	for i := range t.included {
		r, err := e.closingRate(ctx, *t, t.included[i].co)
		if err != nil {
			return err
		}
		t.included[i].rate = r
	}
	return e.acquirable(ctx, tx, *t)
}

// closingRate is the company's closing rate into the group currency
// for the run's period (rateOf).
func (e *Endpoints) closingRate(ctx context.Context, t target, co Company) (Rate, error) {
	return e.rateOf(ctx, t.rateType, t.currency, co, t.period, t.year, t.per)
}

// rateOf is the company's closing rate into the group currency for a
// period (ADR-0059 §7, ADR-0042's rule): 1 in the group currency; else
// the latest of the rate type valid on the period's last day, refused
// when missing (422 consolidation.rate_missing) or valid from before the
// period's first day (422 consolidation.rate_stale).
func (e *Endpoints) rateOf(ctx context.Context, rateType, group string, co Company, p Period, year, per int16) (Rate, error) {
	if co.LocalCurrency == group {
		return Rate{Rate: one(), Shown: "1"}, nil
	}
	end, start := p.End.Format(time.DateOnly), p.Start.Format(time.DateOnly)
	pair := co.LocalCurrency + "/" + group
	r, ok, err := e.Ledger.RateOn(ctx, rateType, co.LocalCurrency, group, p.End)
	switch {
	case err != nil:
		return Rate{}, fmt.Errorf("consolidation: %s rate %s on %s: %w", rateType, pair, end, err)
	case !ok || r.Rate.Sign() <= 0:
		return Rate{}, problem.Unprocessable("consolidation.rate_missing", fmt.Sprintf("No %s rate %s is valid on %s, the closing rate of company %s "+
			"for %d/%d.", rateType, pair, end, co.Code, year, per), problem.FieldError{Field: "period", Code: "rate_missing",
			Message: "record the " + rateType + " rate " + pair + " of " + end})
	case r.ValidFrom.Before(p.Start):
		return Rate{}, problem.Unprocessable("consolidation.rate_stale", fmt.Sprintf("The latest %s rate %s on %s, the closing rate of company %s "+
			"for %d/%d, is valid from %s, before the period starts on %s (ADR-0042).", rateType, pair, end, co.Code, year, per, r.ValidFrom.Format(time.DateOnly),
			start), problem.FieldError{Field: "period", Code: "rate_stale", Message: "record a " + rateType + " rate " + pair + " valid from within the period"})
	}
	return r, nil
}

// inProgress refuses a run while another of the group is queued or
// running (409 consolidation.run_in_progress, its detail and Location
// naming it).
func inProgress(ctx context.Context, tx pgx.Tx, code string) error {
	var other uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `SELECT id, status FROM consolidation.run WHERE group_code = $1 AND status IN ('queued', 'running')`, code).Scan(&other, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("consolidation: runs in progress of %s: %w", code, err)
	}
	p := problem.Conflict("consolidation.run_in_progress", fmt.Sprintf("Run %s of group %s is %s; one run of a group runs at a time.", runPath(other),
		code, status))
	p.Header = http.Header{"Location": {runPath(other)}}
	return p
}

// CreateConsolidationRun implements POST /v1/consolidation-runs (ADR-0059
// §6): the run of a group and regular period, written queued with the
// active configuration's version and its Operation, which the workflows
// dispatcher (or the in-process runner) carries out.
func (e *Endpoints) CreateConsolidationRun(ctx context.Context, req *gen.ConsolidationRunStart, _ gen.CreateConsolidationRunParams) (gen.CreateConsolidationRunRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: configuration: %w", err)
	}
	t, err := e.targetOf(ctx, tx, doc, req.GroupCode, req.FiscalYear, req.Period)
	if err != nil {
		return nil, err
	}
	if err := inProgress(ctx, tx, req.GroupCode); err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	op, err := e.Runs.Start(ctx, runPath(id), req)
	if err != nil {
		return nil, fmt.Errorf("consolidation: operation of run %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start,
			period_end, config_version, group_currency, rate_type, status, operation_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'queued', $13, $14, $14)`, p.TenantID, id, t.group.Code,
		t.companies[t.group.Parent].ID, t.all(), t.year, t.per, t.period.Start, t.period.End, version, t.currency, t.rateType, op.ID, p.ID); err != nil {
		return nil, fmt.Errorf("consolidation: insert run of %s %d/%d: %w", t.group.Code, t.year, t.per, err)
	}
	r, err := readRun(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	// An audit record that cannot be added fails the request, whose
	// response is then the error's.
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: op}, recordStatus(ctx, r, "")
}

// recordStatus audits a run's change of status from before ("" when it
// was queued) and publishes ConsolidationRunChanged (api/asyncapi.yaml).
func recordStatus(ctx context.Context, r Run, before string) error {
	payload := map[string]any{"run_id": r.ID, "group_code": r.GroupCode, "fiscal_year": r.FiscalYear, "fiscal_period": r.Period, "status": r.Status,
		"status_before": nil, "problem": nil, "version": r.Version}
	if before != "" {
		payload["status_before"] = before
	}
	if r.Problem != nil {
		payload["problem"] = map[string]any{"code": r.Problem.Code, "detail": r.Problem.Detail}
	}
	return audit.Record(ctx, "consolidation.run."+r.Status, "consolidation.run", r.ID, payload,
		outbox.Event{Context: "consolidation", Name: "consolidation_run_changed", AggregateType: "consolidation.run", AggregateID: r.ID, Payload: payload})
}
