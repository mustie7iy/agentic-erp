package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A consolidation run's work (ADR-0059 §6): five steps, each one
// transaction in the tenant's scope as the run's requester, which the
// workflow ConsolidationRun calls as its activities through the
// workflows.Consolidation port (or the in-process runner, without a
// workflow engine). Each first reads the run FOR UPDATE, so the steps, a
// discard and later an acceptance or a finalization serialise on it, and
// each is idempotent: run again after a commit whose completion was lost,
// it finds its effect there and does nothing. While the run is queued or
// running each reads its Operation (liveRun): once it is cancelled the
// step refuses with 409 consolidation.run_cancelled, which the workflow
// records by failing the run, as a depreciation run does (ADR-0057 §7).

// Member is a company of a run as run_member records it.
type Member struct {
	LegalEntityID              uuid.UUID
	CompanyCode, Role          string
	OwnershipPercent           *string
	FirstYear, FirstPeriod     *int16
	LocalCurrency, ClosingRate string
	RateValidFrom              *time.Time
	PeriodStatus               *string
	PeriodVersion              *int32
	TranslatedAt               *time.Time
}

// memberColumns are a run member's columns, as scanMember reads them.
const memberColumns = `legal_entity_id, company_code, role, ownership_percent::text, first_fiscal_year, first_fiscal_period, local_currency,
	closing_rate::text, rate_valid_from, period_status, period_version, translated_at`

// memberSelect reads a run's members.
const memberSelect = `SELECT ` + memberColumns + ` FROM consolidation.run_member`

// fields are where a member's columns are read into.
func (m *Member) fields() []any {
	return []any{&m.LegalEntityID, &m.CompanyCode, &m.Role, &m.OwnershipPercent, &m.FirstYear, &m.FirstPeriod, &m.LocalCurrency, &m.ClosingRate,
		&m.RateValidFrom, &m.PeriodStatus, &m.PeriodVersion, &m.TranslatedAt}
}

func scanMember(r pgx.CollectableRow) (Member, error) {
	var m Member
	return m, errs.Wrapf(r.Scan(m.fields()...), "scan consolidation run member")
}

// members reads runs' members, the parent first and then by company code,
// by run.
func members(ctx context.Context, tx pgx.Tx, runs []uuid.UUID) (map[uuid.UUID][]Member, error) {
	rows, _ := tx.Query(ctx, `SELECT run_id, `+memberColumns+` FROM consolidation.run_member WHERE run_id = ANY($1)
		ORDER BY run_id, role = 'member', company_code`, runs)
	out := map[uuid.UUID][]Member{}
	var run uuid.UUID
	var m Member
	_, err := pgx.ForEachRow(rows, append([]any{&run}, m.fields()...), func() error { out[run] = append(out[run], m); return nil })
	return out, errs.Wrapf(err, "consolidation: members of %d runs", len(runs))
}

// lockRun reads a run FOR UPDATE, whoever may act in its companies: the
// run's own work, as its requester.
func lockRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, problem.NotFoundOf("consolidation run")
	}
	return r, errs.Wrapf(err, "consolidation: lock run %s", id)
}

// liveRun locks a run as lockRun does and, while it is queued or running,
// refuses it once its Operation is cancelled (409
// consolidation.run_cancelled). With finishing the Operation is read
// under its lock, for the step that succeeds it; otherwise as it stands,
// so a :cancel during a step commits at once.
func (e *Endpoints) liveRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, finishing bool) (Run, error) {
	r, err := lockRun(ctx, tx, id)
	if err != nil || (r.Status != RunQueued && r.Status != RunRunning) {
		return r, err
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, finishing)
	if err != nil {
		return Run{}, fmt.Errorf("consolidation: the operation of run %s: %w", id, err)
	}
	if cancelled {
		return Run{}, problem.Conflict("consolidation.run_cancelled", fmt.Sprintf("The operation of consolidation run %s was cancelled: the run stopped "+
			"before its next step and failed; start a new run of the period.", id))
	}
	return r, nil
}

// setStatus moves a locked run from its status to another with what else
// changes (set, from $5 on), guarded by the status read (ADR-0059 §5),
// audits it and publishes it.
func setStatus(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, to, set string, args ...any) (Run, error) {
	if _, err := tx.Exec(ctx, `UPDATE consolidation.run SET status = $3`+set+`, version = version + 1, updated_by = $2 WHERE id = $1 AND status = $4`,
		append([]any{r.ID, p.ID, to, r.Status}, args...)...); err != nil {
		return Run{}, fmt.Errorf("consolidation: run %s to %s: %w", r.ID, to, err)
	}
	after, err := lockRun(ctx, tx, r.ID)
	if err != nil {
		return Run{}, err
	}
	return after, recordStatus(ctx, after, r.Status)
}

// PrepareConsolidationRun prepares a queued run: under the configuration
// version it recorded, it checks again what it consolidates (targetOf),
// sets it running and writes its companies with their closing rates. It
// returns the companies to translate by company code; a run already
// running returns those it wrote, one computed, failed or discarded none.
func (e *Endpoints) PrepareConsolidationRun(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := e.liveRun(ctx, tx, id, false)
	if err != nil || (r.Status != RunQueued && r.Status != RunRunning) {
		return nil, err
	}
	if r.Status == RunQueued {
		if err := e.prepare(ctx, tx, p, r); err != nil {
			return nil, err
		}
	}
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id FROM consolidation.run_member WHERE run_id = $1 ORDER BY company_code`, id)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return ids, errs.Wrapf(err, "consolidation: companies of run %s", id)
}

// prepare sets a queued run running with its companies.
func (e *Endpoints) prepare(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run) error {
	doc, err := e.version(ctx, r.ConfigVersion)
	if err != nil {
		return fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	t, err := e.targetOf(ctx, doc, r.GroupCode, int32(r.FiscalYear), int32(r.Period))
	if err != nil {
		return err
	}
	if _, err := setStatus(ctx, tx, p, r, RunRunning, ""); err != nil {
		return err
	}
	for _, c := range t.included {
		role, ownership, year, period, from := "parent", (*string)(nil), (*int16)(nil), (*int16)(nil), (*time.Time)(nil)
		if c.member != nil {
			role, ownership, year, period = "member", &c.member.OwnershipPercent, &c.firstYear, &c.firstP
		}
		if c.co.LocalCurrency != t.currency {
			from = &c.rate.ValidFrom
		}
		if _, err := tx.Exec(ctx, `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, ownership_percent,
				first_fiscal_year, first_fiscal_period, local_currency, closing_rate, rate_valid_from) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			p.TenantID, r.ID, c.co.ID, c.co.Code, role, ownership, year, period, c.co.LocalCurrency, c.rate.Shown, from); err != nil {
			return fmt.Errorf("consolidation: company %s of run %s: %w", c.co.Code, r.ID, err)
		}
	}
	return errs.Wrapf(e.Runs.Report(ctx, r.OperationID, 0, int64(len(t.included))), "consolidation: progress of run %s", r.ID)
}

// TranslateMember translates one company of a running run in one
// transaction (ADR-0059 §7): it reads the status and version of the run's
// period in the company's leading ledger first and then its balances
// through it (two statements of a read-committed transaction), so the
// version recorded is never newer than the figures read and a change
// between the two reads makes the run stale (T68g), never wrongly fresh; writes its translated rows; records the period and
// when; and reports the run's progress. A company already translated, or
// a run no longer running, is left alone.
func (e *Endpoints) TranslateMember(ctx context.Context, id, company uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := e.liveRun(ctx, tx, id, false)
	if err != nil || r.Status != RunRunning {
		return err
	}
	rows, _ := tx.Query(ctx, memberSelect+` WHERE run_id = $1 AND legal_entity_id = $2`, id, company)
	m, err := pgx.CollectExactlyOneRow(rows, scanMember)
	if err != nil {
		return fmt.Errorf("consolidation: company %s of run %s: %w", company, id, err)
	}
	if m.TranslatedAt != nil {
		return nil
	}
	per, ok, err := e.Ledger.PeriodOf(ctx, company, r.FiscalYear, r.Period)
	switch {
	case err != nil:
		return fmt.Errorf("consolidation: period %d/%d of %s: %w", r.FiscalYear, r.Period, m.CompanyCode, err)
	case !ok:
		return problem.Unprocessable("consolidation.period_unknown", fmt.Sprintf("Company %s has no period %d/%d in its leading ledger.", m.CompanyCode,
			r.FiscalYear, r.Period), problem.FieldError{Field: "period", Code: "period_unknown", Message: "a period of company " + m.CompanyCode})
	}
	f, err := e.figuresOf(ctx, tx, r, m)
	if err != nil {
		return err
	}
	out, err := translate(f)
	if err != nil {
		return err
	}
	if err := writeRows(ctx, tx, p, r.ID, company, out); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE consolidation.run_member SET period_status = $3, period_version = $4, translated_at = now()
		WHERE run_id = $1 AND legal_entity_id = $2`, id, company, per.Status, per.Version); err != nil {
		return fmt.Errorf("consolidation: record the translation of %s in run %s: %w", m.CompanyCode, id, err)
	}
	return e.progress(ctx, tx, r)
}

// figuresOf reads what a company's translation needs (figures).
func (e *Endpoints) figuresOf(ctx context.Context, tx pgx.Tx, r Run, m Member) (figures, error) {
	doc, err := e.version(ctx, r.ConfigVersion)
	if err != nil {
		return figures{}, fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	f := figures{company: Company{ID: m.LegalEntityID, Code: m.CompanyCode, LocalCurrency: m.LocalCurrency}, year: r.FiscalYear,
		chart: doc.Consolidation, mapping: map[string]string{}, accounts: map[uuid.UUID]Account{}, partners: map[uuid.UUID]string{}}
	if m.FirstYear != nil {
		f.firstYear, f.firstP = *m.FirstYear, *m.FirstPeriod
	}
	if co := doc.Company(m.CompanyCode); co != nil {
		for _, a := range co.ChartOfAccounts {
			f.mapping[a.Number] = a.GroupAccount
		}
	}
	f.rate = money.ParseRat(m.ClosingRate) // a numeric(28,12) above zero, always a decimal
	if f.expLC, err = currency.Exponent(ctx, tx, m.LocalCurrency, nil); err != nil {
		return figures{}, fmt.Errorf("consolidation: currency %s: %w", m.LocalCurrency, err)
	}
	if f.expGC, err = currency.Exponent(ctx, tx, r.GroupCurrency, nil); err != nil {
		return figures{}, fmt.Errorf("consolidation: currency %s: %w", r.GroupCurrency, err)
	}
	accounts, err := e.Ledger.Accounts(ctx, m.LegalEntityID)
	if err != nil {
		return figures{}, fmt.Errorf("consolidation: accounts of %s: %w", m.CompanyCode, err)
	}
	for _, a := range accounts {
		f.accounts[a.ID] = a
	}
	entities, err := e.Companies.Entities(ctx)
	if err != nil {
		return figures{}, fmt.Errorf("consolidation: companies: %w", err)
	}
	for _, c := range entities {
		f.partners[c.ID] = c.Code
	}
	f.balances, err = e.Ledger.BalancesThrough(ctx, m.LegalEntityID, r.FiscalYear, r.Period)
	return f, errs.Wrapf(err, "consolidation: balances of %s through %d/%d", m.CompanyCode, r.FiscalYear, r.Period)
}

// writeRows writes a company's translated rows, numbered in their order,
// in one statement.
func writeRows(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run, company uuid.UUID, rows []row) error {
	n := len(rows)
	nos, groups, kinds := make([]int32, n), make([]string, n), make([]string, n)
	accounts, partners := make([]pgtype.UUID, n), make([]pgtype.UUID, n)
	lc, booked, gc := make([]pgtype.Int8, n), make([]pgtype.Int8, n), make([]int64, n)
	for i, r := range rows {
		nos[i], groups[i], kinds[i], gc[i] = int32(i+1), r.groupAccount, r.kind, int64(r.gc)
		if r.account != nil {
			accounts[i] = pgtype.UUID{Bytes: r.account.ID, Valid: true}
		}
		if r.partner != nil {
			partners[i] = pgtype.UUID{Bytes: *r.partner, Valid: true}
		}
		if r.lc != nil {
			lc[i], booked[i] = pgtype.Int8{Int64: int64(*r.lc), Valid: true}, pgtype.Int8{Int64: int64(*r.gcBooked), Valid: true}
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO consolidation.run_balance (tenant_id, run_id, legal_entity_id, line_no, group_account, account_id,
			trading_partner_id, kind, amount_lc, amount_gc_booked, amount_gc)
		SELECT $1, $2, $3, x.n, x.g, x.a, x.t, x.k, x.lc, x.gb, x.gc
		FROM unnest($4::int[], $5::text[], $6::uuid[], $7::uuid[], $8::text[], $9::bigint[], $10::bigint[], $11::bigint[]) AS x(n, g, a, t, k, lc, gb, gc)`,
		p.TenantID, run, company, nos, groups, accounts, partners, kinds, lc, booked, gc)
	return errs.Wrapf(err, "consolidation: translated balances of %s in run %s", company, run)
}

// progress reports how many of the run's companies are translated on its
// Operation.
func (e *Endpoints) progress(ctx context.Context, tx pgx.Tx, r Run) error {
	var done, total int64
	if err := tx.QueryRow(ctx, `SELECT count(translated_at), count(*) FROM consolidation.run_member WHERE run_id = $1`, r.ID).Scan(&done, &total); err != nil {
		return fmt.Errorf("consolidation: progress of run %s: %w", r.ID, err)
	}
	return errs.Wrapf(e.Runs.Report(ctx, r.OperationID, done, total), "consolidation: progress of run %s", r.ID)
}

// EliminateConsolidationRun matches and eliminates a running run's
// intercompany balances and its investments (ADR-0059 §8, §9): T68f and
// T68h write them; until then it checks the run and writes nothing, so
// the workflow's steps are those it will keep.
func (e *Endpoints) EliminateConsolidationRun(ctx context.Context, id uuid.UUID) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	_, err = e.liveRun(ctx, tx, id, false)
	return err
}

// CompleteConsolidationRun sets a running run whose companies are all
// translated computed, audits and publishes it and succeeds its
// Operation. A run no longer running is left alone; one whose Operation
// was cancelled is refused (liveRun), so it is failed rather than
// computed.
func (e *Endpoints) CompleteConsolidationRun(ctx context.Context, id uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := e.liveRun(ctx, tx, id, true)
	if err != nil || r.Status != RunRunning {
		return err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM consolidation.run_member WHERE run_id = $1 AND translated_at IS NULL`, id).Scan(&pending); err != nil {
		return fmt.Errorf("consolidation: companies of run %s: %w", id, err)
	}
	if pending > 0 {
		return fmt.Errorf("consolidation: run %s has %d companies not translated; it is computed after them", id, pending)
	}
	if r, err = setStatus(ctx, tx, p, r, RunComputed, ", computed_at = now()"); err != nil {
		return err
	}
	return errs.Wrapf(e.Runs.Succeed(ctx, r.OperationID, runPath(id), map[string]any{"run_id": id, "status": r.Status}),
		"consolidation: succeed the operation of run %s", id)
}

// FailConsolidationRun sets a queued or running run failed with the
// problem that stopped it, audits and publishes it and fails its
// Operation with the problem; rows already written stay. A run in any
// other status is left alone. An Operation cancelled is left cancelled,
// its final status (04 §4.8): the run that stopped for it records the
// cancellation as its problem.
func (e *Endpoints) FailConsolidationRun(ctx context.Context, id uuid.UUID, why *problem.Problem) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := lockRun(ctx, tx, id)
	if err != nil || (r.Status != RunQueued && r.Status != RunRunning) {
		return err
	}
	failure, _ := json.Marshal(RunProblem{Code: why.Code, Detail: why.Detail}) // two strings always encode
	if r, err = setStatus(ctx, tx, p, r, RunFailed, ", problem = $5", failure); err != nil {
		return err
	}
	cancelled, err := e.Runs.Cancelled(ctx, r.OperationID, true)
	if err != nil || cancelled {
		return errs.Wrapf(err, "consolidation: the operation of run %s", id)
	}
	return errs.Wrapf(e.Runs.Fail(ctx, r.OperationID, why), "consolidation: fail the operation of run %s", id)
}

// UnfinishedConsolidationRuns lists the runs queued or running in the
// transaction's tenant, by run with its Operation: the work of the
// in-process runner of a cell without a workflow engine, which takes up a
// run whose Operation was cancelled too, so that its next step fails it.
func (e *Endpoints) UnfinishedConsolidationRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id, operation_id FROM consolidation.run WHERE status IN ('queued', 'running')`)
	out := map[uuid.UUID]uuid.UUID{}
	var run, op uuid.UUID
	_, err := pgx.ForEachRow(rows, []any{&run, &op}, func() error { out[run] = op; return nil })
	return out, errs.Wrapf(err, "consolidation: unfinished runs")
}

// plainRate is a stored rate or percentage, a numeric, as a plain
// decimal without trailing zeros.
func plainRate(stored string) string { return money.PlainQuantity(money.ParseRat(stored)) }
