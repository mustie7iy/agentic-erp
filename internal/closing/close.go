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
	// The contract bounds the year to 1900-2999 and the period to 1-16.
	year, period := int16(req.FiscalYear), int16(req.FiscalPeriod)
	tx, c, err := e.openClose(ctx, periodCloses, req.LegalEntityID, opening{
		checklist: func(ctx context.Context, doc *config.Config, co Company) ([]LedgerPeriod, []config.CloseTask, string, error) {
			if period > maxRegular {
				return nil, nil, "", problem.Unprocessable("closing.special_period", fmt.Sprintf("Period %d is a special period, the year close's; "+
					"a close is of a regular period (1-12).", period), problem.FieldError{Field: "fiscal_period", Code: "special", Message: "a regular period, 1-12"})
			}
			covered, err := e.covered(ctx, co.ID, year, period, req.LedgerIds)
			return covered, doc.CloseTasksOf(co.Code), "close template (close.tasks) gives", err
		},
		insert: func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, end time.Time, version int32, covered []LedgerPeriod) error {
			return insertPeriodClose(ctx, tx, p, id, req.LegalEntityID, year, period, end, version, covered)
		},
	})
	if err != nil {
		return nil, err
	}
	out, err := e.closeOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.PeriodCloseHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(periodCloses.path + c.ID.String()),
		Response: out}, nil
}

// opening is what a kind of close is opened with, which its operation
// gives openClose. checklist reads the ledgers it covers, each with the
// period its due dates count from (a period close's period, a year close's
// last regular period), the template's tasks for its kind and company, and
// the template's name for a refusal; insert stores it once no close of its
// period or year exists. Each operation passes its own, so the problems
// one kind answers are not the other's (api TestProblemCodes).
type opening struct {
	checklist func(ctx context.Context, doc *config.Config, co Company) (covered []LedgerPeriod, tasks []config.CloseTask, template string, err error)
	insert    func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, end time.Time, version int32, covered []LedgerPeriod) error
}

// openClose opens a close of a kind (ADR-0058 §6, ADR-0060 §8) of a
// company, covering the ledgers its checklist names, its tasks the
// template's for its kind and company, which must give it at least one,
// copied with their due dates and the configuration version; audited and
// published. It returns the request's transaction and the close.
func (e *Endpoints) openClose(ctx context.Context, k *closeKind, legalEntity uuid.UUID, o opening) (pgx.Tx, closeRow, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, closeRow{}, err
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, closeRow{}, fmt.Errorf("closing: open %s: %w", k.noun, err)
	}
	co, err := e.companyOf(ctx, legalEntity)
	if err != nil {
		return nil, closeRow{}, err
	}
	covered, tasks, template, err := o.checklist(ctx, doc, co)
	if err != nil {
		return nil, closeRow{}, err
	}
	if len(tasks) == 0 {
		return nil, closeRow{}, problem.Conflict("closing.no_template", "The active configuration's "+template+" company "+co.Code+
			" no task; add tasks for it through a configuration change first.")
	}
	id := uuid.Must(uuid.NewV7())
	end := covered[0].Period.EndDate // the leading ledger's, which covered puts first
	if err := o.insert(ctx, tx, p, id, end, version, covered); err != nil {
		return nil, closeRow{}, err
	}
	if err := insertTasks(ctx, tx, k, p, id, plan(doc, co.Code, end, tasks)); err != nil {
		return nil, closeRow{}, err
	}
	c, err := readClose(ctx, tx, k, id)
	if err != nil {
		return nil, closeRow{}, err
	}
	return tx, c, record(ctx, k, c, change{action: "create", name: "created", detail: map[string]any{"config_version": version}})
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

// covered are the ledgers a period close covers, the leading one first,
// then by code, each with its period (pick).
func (e *Endpoints) covered(ctx context.Context, legalEntity uuid.UUID, year, period int16, ids []uuid.UUID) ([]LedgerPeriod, error) {
	kept, err := e.Ledger.PeriodsOf(ctx, legalEntity, year, period)
	if err != nil {
		return nil, fmt.Errorf("closing: ledgers of %s: %w", legalEntity, err)
	}
	return pick(kept, ids, fmt.Sprintf("period %d/%d", year, period), "fiscal_period")
}

// pick are the ledgers a close covers among those its company keeps
// (kept, the leading one first, then by code, each with the period the
// close counts its due dates from, nil when the ledger lacks what, the
// close's period or year): those the request names, every one the company
// keeps when it names none (a refusal then names field). Each must be kept
// and have its period, and the leading one must be among them: a close's
// due dates count from its period, and its sign-off locks it first
// (ADR-0058 §5, §7; ADR-0060 §8).
func pick(kept []LedgerPeriod, ids []uuid.UUID, what, field string) ([]LedgerPeriod, error) {
	at := func(i int) string { return fmt.Sprintf("ledger_ids[%d]", i) }
	if len(ids) == 0 {
		at = func(int) string { return field }
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
				problem.FieldError{Field: at(i), Code: "not_kept", Message: "a ledger the legal entity keeps"})
		case kept[j].Period == nil:
			return nil, problem.Unprocessable("closing.period_unknown", fmt.Sprintf("Ledger %s has no %s for the company.", kept[j].Code, what),
				problem.FieldError{Field: at(i), Code: "period_unknown", Message: "a ledger with " + what + ", not " + kept[j].Code})
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

// plan copies the template's tasks for a company's close of a period or
// a year ending on end, each due on the company's business day its due day
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

// insertPeriodClose writes a period close and the ledgers it covers, each
// with its period, refusing a second close of the company's period.
func insertPeriodClose(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id, legalEntity uuid.UUID, year, period int16, end time.Time, version int32,
	covered []LedgerPeriod) error {
	first, ok, err := existing(ctx, tx, periodCloses, legalEntity, year, period)
	if err != nil {
		return err
	}
	if ok {
		return exists(periodCloses, first, problem.Conflict("closing.close_exists",
			fmt.Sprintf("The company has close %s of period %d/%d already.", first, year, period)))
	}
	ledgers, periods := make([]uuid.UUID, len(covered)), make([]uuid.UUID, len(covered))
	for i, l := range covered {
		ledgers[i], periods[i] = l.LedgerID, l.Period.ID
	}
	_, err = tx.Exec(ctx, `WITH c AS (INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end,
			config_version, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8))
		INSERT INTO closing.close_ledger (tenant_id, close_id, ledger_id, period_id) SELECT $1, $2, l, p FROM unnest($9::uuid[], $10::uuid[]) AS x(l, p)`,
		p.TenantID, id, legalEntity, year, period, end, version, p.ID, ledgers, periods)
	return errs.Wrapf(err, "closing: insert close of %s, %d/%d", legalEntity, year, period)
}

// insertTasks writes the tasks of a close of a kind in one statement.
func insertTasks(ctx context.Context, tx pgx.Tx, k *closeKind, p reqctx.Principal, closeID uuid.UUID, tasks []planned) error {
	rows, _ := json.Marshal(tasks) // strings, numbers and lists of strings always encode
	_, err := tx.Exec(ctx, k.q(`INSERT INTO {task} (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, depends_on, created_by, updated_by)
		SELECT $1, $2, x.code, x.seq, x.name, x.kind, x.check, x.due_on, x.depends_on, $3, $3
		FROM jsonb_to_recordset($4::jsonb) AS x(code text, seq smallint, name text, kind text, "check" text, due_on date, depends_on text[])`),
		p.TenantID, closeID, p.ID, rows)
	return errs.Wrapf(err, "closing: insert tasks of "+k.noun+" %s", closeID)
}

// closeOut is a close as a single read shows it: with its company, its
// ledgers' live period status, its tasks and its sign-offs.
func (e *Endpoints) closeOut(ctx context.Context, tx pgx.Tx, c closeRow) (gen.PeriodClose, error) {
	tasks, err := tasksOf(ctx, tx, periodCloses, c.ID)
	if err != nil {
		return gen.PeriodClose{}, err
	}
	offs, err := signOffsOf(ctx, tx, periodCloses, c.ID)
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
func (e *Endpoints) ledgersOf(ctx context.Context, tx pgx.Tx, c closeRow) ([]gen.CloseLedger, error) {
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
	covered, err := coveredIDs(ctx, tx, periodCloses, closeID)
	if err != nil {
		return nil, err
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

// coveredIDs are the ledgers a close of a kind covers.
func coveredIDs(ctx context.Context, tx pgx.Tx, k *closeKind, closeID uuid.UUID) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, k.q(`SELECT ledger_id FROM {ledger} WHERE close_id = $1`), closeID)
	covered, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("closing: ledgers of %s %s: %w", k.noun, closeID, err)
	}
	return covered, nil
}

// GetPeriodClose implements GET /v1/period-closes/{id}.
func (e *Endpoints) GetPeriodClose(ctx context.Context, params gen.GetPeriodCloseParams) (gen.GetPeriodCloseRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := readClose(ctx, tx, periodCloses, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.closeOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.GetPeriodCloseOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}
