package closing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// maxRegular is the last regular period; 13-16 are special periods, the
// year-end close's, which T64 leaves out (ADR-0058 §1).
const maxRegular = 12

// CreatePeriodClose implements POST /v1/period-closes (ADR-0058 §6): the
// close of a company's regular period, covering the ledgers named (every
// one the company keeps by default, the leading one always), its tasks
// the active close template's for the company, copied with their due
// dates and the configuration version.
func (e *Endpoints) CreatePeriodClose(ctx context.Context, req *gen.PeriodCloseCreate, _ gen.CreatePeriodCloseParams) (gen.CreatePeriodCloseRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("closing: open close: %w", err)
	}
	co, err := e.companyOf(ctx, req.LegalEntityID)
	if err != nil {
		return nil, err
	}
	// The contract bounds the year to 1900-2999 and the period to 1-16.
	year, period := int16(req.FiscalYear), int16(req.FiscalPeriod)
	if period > maxRegular {
		return nil, problem.Unprocessable("closing.special_period", fmt.Sprintf("Period %d is a special period, the year-end close's; a close is of a regular period (1-12).",
			period), problem.FieldError{Field: "fiscal_period", Code: "special", Message: "a regular period, 1-12"})
	}
	covered, err := e.covered(ctx, req.LegalEntityID, year, period, req.LedgerIds)
	if err != nil {
		return nil, err
	}
	tasks := doc.CloseTasksOf(co.Code)
	if len(tasks) == 0 {
		return nil, problem.Conflict("closing.no_template", "The active configuration's close template (close.tasks) gives company "+co.Code+
			" no task; add tasks for it through a configuration change first.")
	}
	if err := closeOf(ctx, tx, req.LegalEntityID, year, period); err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	end := covered[0].Period.EndDate // the leading ledger's, which covered puts first
	if err := insertClose(ctx, tx, p, id, req.LegalEntityID, year, period, end, version, covered); err != nil {
		return nil, err
	}
	if err := insertTasks(ctx, tx, p, id, plan(doc, co.Code, end, tasks)); err != nil {
		return nil, err
	}
	c, err := readClose(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := record(ctx, c, change{action: "create", name: "created", detail: map[string]any{"config_version": version}}); err != nil {
		return nil, err
	}
	out, err := e.closeOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.PeriodCloseHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/period-closes/" + id.String()),
		Response: out}, nil
}

// companyOf is the company of a legal entity the request names, which
// must exist and be one the caller may act in.
func (e *Endpoints) companyOf(ctx context.Context, legalEntity uuid.UUID) (Company, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Company{}, err
	}
	co, ok, err := e.Companies.Company(ctx, legalEntity)
	if err != nil {
		return Company{}, fmt.Errorf("closing: company of %s: %w", legalEntity, err)
	}
	if !ok || !access.Allows(legalEntity) {
		return Company{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id that you can access")
	}
	return co, nil
}

// covered are the ledgers a close covers, the leading one first, then by
// code, each with its period: those the request names, every one the
// company keeps when it names none. Each must be kept and have the period,
// and the leading one must be among them: a close's due dates count from
// its period, and its sign-off locks it first (ADR-0058 §5, §7).
func (e *Endpoints) covered(ctx context.Context, legalEntity uuid.UUID, year, period int16, ids []uuid.UUID) ([]LedgerPeriod, error) {
	kept, err := e.Ledger.PeriodsOf(ctx, legalEntity, year, period)
	if err != nil {
		return nil, fmt.Errorf("closing: ledgers of %s: %w", legalEntity, err)
	}
	field := func(i int) string { return fmt.Sprintf("ledger_ids[%d]", i) }
	if len(ids) == 0 {
		field = func(int) string { return "fiscal_period" }
		for _, k := range kept {
			ids = append(ids, k.LedgerID)
		}
	}
	named := map[uuid.UUID]bool{}
	for i, id := range ids {
		j := slices.IndexFunc(kept, func(k LedgerPeriod) bool { return k.LedgerID == id })
		switch {
		case j < 0:
			return nil, problem.Unprocessable("ledger.ledger_not_kept", "The legal entity does not keep ledger "+id.String()+"; a close covers ledgers its company keeps.",
				problem.FieldError{Field: field(i), Code: "not_kept", Message: "a ledger the legal entity keeps"})
		case kept[j].Period == nil:
			return nil, problem.Unprocessable("closing.period_unknown", fmt.Sprintf("Ledger %s has no period %d/%d for the company.", kept[j].Code, year, period),
				problem.FieldError{Field: field(i), Code: "period_unknown", Message: "a ledger with the period, not " + kept[j].Code})
		}
		named[id] = true // the contract's uniqueItems names each once
	}
	// kept is in the leading-first, then code order, and out keeps it.
	var out []LedgerPeriod
	for _, k := range kept {
		if named[k.LedgerID] {
			out = append(out, k)
		}
	}
	if len(out) == 0 || !out[0].Leading {
		return nil, problem.Unprocessable("request.validation_failed", "A close covers the leading ledger, whose period its due dates count from.",
			problem.FieldError{Field: "ledger_ids", Code: "leading_missing", Message: "the leading ledger among the ledgers"})
	}
	return out, nil
}

// planned is a template task as a close is given it: with its place in the
// template's order, its check (nil for a manual task) and its due date in
// the close's period.
type planned struct {
	Code      string   `json:"code"`
	Seq       int      `json:"seq"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Check     *string  `json:"check"`
	DueOn     string   `json:"due_on"`
	DependsOn []string `json:"depends_on"`
}

// plan copies the template's tasks for a company's close of a period
// ending on end, each due on the company's business day its due day
// counts to, as the calendar plans it.
func plan(doc *config.Config, company string, end time.Time, tasks []config.CloseTask) []planned {
	out := make([]planned, len(tasks))
	for i, t := range tasks {
		out[i] = planned{Code: t.Code, Seq: i + 1, Name: t.Name, Kind: t.Kind, DueOn: dueOn(doc, company, end, t.DueDay).Format(time.DateOnly),
			DependsOn: append([]string{}, t.DependsOn...)}
		if t.Kind == config.CloseCheck {
			out[i].Check = &t.Check
		}
	}
	return out
}

// insertClose writes a close and the ledgers it covers.
func insertClose(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id, legalEntity uuid.UUID, year, period int16, end time.Time, version int32,
	covered []LedgerPeriod) error {
	if _, err := tx.Exec(ctx, `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, p.TenantID, id, legalEntity, year, period, end, version, p.ID); err != nil {
		return fmt.Errorf("closing: insert close of %s, %d/%d: %w", legalEntity, year, period, err)
	}
	ledgers, periods := make([]uuid.UUID, len(covered)), make([]uuid.UUID, len(covered))
	for i, k := range covered {
		ledgers[i], periods[i] = k.LedgerID, k.Period.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO closing.close_ledger (tenant_id, close_id, ledger_id, period_id)
		SELECT $1, $2, l, p FROM unnest($3::uuid[], $4::uuid[]) AS x(l, p)`, p.TenantID, id, ledgers, periods)
	return errs.Wrapf(err, "closing: insert ledgers of close %s", id)
}

// insertTasks writes a close's tasks in one statement.
func insertTasks(ctx context.Context, tx pgx.Tx, p reqctx.Principal, closeID uuid.UUID, tasks []planned) error {
	rows, _ := json.Marshal(tasks) // strings, numbers and lists of strings always encode
	_, err := tx.Exec(ctx, `INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, depends_on, created_by, updated_by)
		SELECT $1, $2, x.code, x.seq, x.name, x.kind, x.check, x.due_on, x.depends_on, $3, $3
		FROM jsonb_to_recordset($4::jsonb) AS x(code text, seq smallint, name text, kind text, "check" text, due_on date, depends_on text[])`,
		p.TenantID, closeID, p.ID, rows)
	return errs.Wrapf(err, "closing: insert tasks of close %s", closeID)
}

// closeOut is a close as a single read shows it: with its company, its
// ledgers' live period status, its tasks and its sign-offs.
func (e *Endpoints) closeOut(ctx context.Context, tx pgx.Tx, c periodClose) (gen.PeriodClose, error) {
	tasks, err := tasksOf(ctx, tx, c.ID)
	if err != nil {
		return gen.PeriodClose{}, err
	}
	offs, err := signOffsOf(ctx, tx, c.ID)
	if err != nil {
		return gen.PeriodClose{}, err
	}
	v, err := e.view(ctx, []uuid.UUID{c.LegalEntityID}, tasks, idref.Of(offs, func(s signOff) *uuid.UUID { return &s.SignedBy })...)
	if err != nil {
		return gen.PeriodClose{}, err
	}
	ledgers, err := e.ledgersOf(ctx, tx, c)
	if err != nil {
		return gen.PeriodClose{}, err
	}
	return v.closeToAPI(c, ledgers, tasks, offs), nil
}

// ledgersOf are the ledgers a close covers, with their periods' live
// status, the leading one first, then by code.
func (e *Endpoints) ledgersOf(ctx context.Context, tx pgx.Tx, c periodClose) ([]gen.CloseLedger, error) {
	covered, err := e.coveredPeriods(ctx, tx, c.ID, c.LegalEntityID, c.FiscalYear, c.FiscalPeriod)
	if err != nil {
		return nil, err
	}
	out := []gen.CloseLedger{}
	for _, k := range covered {
		out = append(out, gen.CloseLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, IsLeading: k.Leading,
			FiscalPeriodID: k.Period.ID, PeriodStatus: k.Period.Status})
	}
	return out, nil
}

// coveredPeriods are the ledgers a close covers, each with its period as
// the ledger holds it now, the leading one first, then by code.
func (e *Endpoints) coveredPeriods(ctx context.Context, tx pgx.Tx, closeID, legalEntity uuid.UUID, year, period int16) ([]LedgerPeriod, error) {
	rows, _ := tx.Query(ctx, `SELECT ledger_id FROM closing.close_ledger WHERE close_id = $1`, closeID)
	covered, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("closing: ledgers of close %s: %w", closeID, err)
	}
	kept, err := e.Ledger.PeriodsOf(ctx, legalEntity, year, period)
	if err != nil {
		return nil, fmt.Errorf("closing: periods of close %s: %w", closeID, err)
	}
	var out []LedgerPeriod
	for _, k := range kept {
		if slices.Contains(covered, k.LedgerID) && k.Period != nil {
			out = append(out, k)
		}
	}
	return out, nil
}

// GetPeriodClose implements GET /v1/period-closes/{id}.
func (e *Endpoints) GetPeriodClose(ctx context.Context, params gen.GetPeriodCloseParams) (gen.GetPeriodCloseRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := readClose(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.closeOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetPeriodCloseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}
