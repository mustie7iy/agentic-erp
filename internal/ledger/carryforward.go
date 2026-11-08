package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Carry-forward bases and row kinds (ADR-0059 §5).
const (
	BasisChained  = "chained"
	BasisAllYears = "all_years"
	KindBalance   = "balance"
	KindResult    = "result"
)

// keptLedger is a ledger a carry-forward runs in, in the one lock order:
// the leading ledger first, then by code.
type keptLedger struct {
	id         uuid.UUID
	code, name string
}

// readPeriod is a posting period a carry-forward read, with its status and
// opened count when it read it.
type readPeriod struct {
	id, ledger   uuid.UUID
	year, period int16
	status       string
	opened       int32
}

// carryForwardRequest is what one carry-forward run reads, besides its
// ledger.
type carryForwardRequest struct {
	entity           tenancy.Entity
	group            string // the group currency
	year             int16
	retainedEarnings AccountRef
}

// RunBalanceCarryForward implements POST /v1/balance-carry-forwards: the
// carry-forward of a company's fiscal year into the next in each ledger
// named, one run per ledger in one transaction (ADR-0059 §5). People only.
func (e *Endpoints) RunBalanceCarryForward(ctx context.Context, req *gen.BalanceCarryForwardRequest, _ gen.RunBalanceCarryForwardParams) (gen.RunBalanceCarryForwardRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, err := e.visibleEntity(ctx, req.LegalEntityID)
	if err != nil {
		return nil, err
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return nil, err
	}
	r := carryForwardRequest{entity: le, group: group, year: int16(req.FiscalYear)}
	ledgers, err := carryForwardLedgers(ctx, tx, le.ID, req.LedgerIds)
	if err != nil {
		return nil, err
	}
	if r.retainedEarnings, err = retainedEarnings(ctx, tx, le); err != nil {
		return nil, err
	}
	periods, err := lockCarryForward(ctx, tx, le.ID, ledgers, r.year)
	if err != nil {
		return nil, err
	}
	if err := carryForwardReady(ctx, tx, r, ledgers, periods); err != nil {
		return nil, err
	}
	out := gen.BalanceCarryForwardRuns{Items: make([]gen.BalanceCarryForward, 0, len(ledgers))}
	for _, l := range ledgers {
		run, err := carryForward(ctx, tx, p, r, l, periods)
		if err != nil {
			return nil, err
		}
		rep, err := e.carryForwardToAPI(ctx, tx, run)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, rep)
	}
	return &gen.BalanceCarryForwardRunsHeaders{Response: out}, nil
}

// visibleEntity is the legal entity of a request's legal_entity_id, one
// the caller may act in; any other is refused as a reference to none.
func (e *Endpoints) visibleEntity(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	if err != nil {
		return tenancy.Entity{}, err
	}
	if !ok || !access.Allows(le.ID) {
		return tenancy.Entity{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id")
	}
	return le, nil
}

// carryForwardLedgers are the ledgers named, or every ledger the company
// keeps when none is, in the one lock order; a ledger that does not exist
// or that the company does not keep is refused.
func carryForwardLedgers(ctx context.Context, tx pgx.Tx, le uuid.UUID, named []uuid.UUID) ([]keptLedger, error) {
	rows, _ := tx.Query(ctx, `SELECT id, code, name, ledger.company_keeps($1, id) FROM ledger.ledger
		WHERE CASE WHEN $2::uuid[] IS NULL THEN ledger.company_keeps($1, id) ELSE id = ANY($2) END ORDER BY NOT is_leading, code`, le, named)
	var ledgers []keptLedger
	var l keptLedger
	kept, all := false, true
	_, err := pgx.ForEachRow(rows, []any{&l.id, &l.code, &l.name, &kept}, func() error {
		ledgers, all = append(ledgers, l), all && kept
		return nil
	})
	switch {
	case err != nil:
		return nil, fmt.Errorf("ledger: carry-forward of %s: ledgers: %w", le, err)
	case named != nil && len(ledgers) < len(named):
		return nil, problem.ReferenceNotFound("ledger_ids", "ledger", "ledgers that exist")
	case !all:
		return nil, notKept("ledger_ids")
	}
	return ledgers, nil
}

// retainedEarnings is the company's retained earnings account (ADR-0059
// §4), one for every ledger it keeps; without one the carry-forward is
// refused.
func retainedEarnings(ctx context.Context, tx pgx.Tx, le tenancy.Entity) (AccountRef, error) {
	var a AccountRef
	err := tx.QueryRow(ctx, `SELECT a.id, a.number, a.name FROM ledger.account_determination d JOIN ledger.gl_account a ON a.tenant_id = d.tenant_id
		AND a.id = d.account_id WHERE d.legal_entity_id = $1 AND d.key = 'retained_earnings'`, le.ID).Scan(&a.ID, &a.Number, &a.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return a, problem.Conflict("ledger.retained_earnings_missing", "Company "+le.Code+" has no retained earnings account to carry a year's result to; "+
			"set companies[].year_end.retained_earnings_account in the configuration first.")
	case err != nil:
		return a, fmt.Errorf("ledger: retained earnings account of %s: %w", le.ID, err)
	}
	return a, nil
}

// lockCarryForward takes the carry-forward's locks (ADR-0059 §5): first a
// transaction-scoped advisory lock per ledger run, in the ledgers' order,
// so two runs of a company, ledger and year wait for each other; then
// every period of the year and of every earlier year of the ledgers, FOR
// SHARE, in the one order (by year and period, the leading ledger first,
// then by code): a posting into one needs it open, which OpenPeriod's FOR
// UPDATE waits for, so the run sees every committed posting. It returns
// the periods read.
func lockCarryForward(ctx context.Context, tx pgx.Tx, le uuid.UUID, ledgers []keptLedger, year int16) ([]readPeriod, error) {
	ids := make([]uuid.UUID, len(ledgers))
	for i, l := range ledgers {
		ids[i] = l.id
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ledger.carry_forward:' || platform.require_tenant() || ':' || $1::text
			|| ':' || l || ':' || $3::smallint, 0)) FROM unnest($2::uuid[]) AS x(l)`, le, ids, year); err != nil {
		return nil, fmt.Errorf("ledger: carry-forward of %s from %d: lock the runs: %w", le, year, err)
	}
	rows, _ := tx.Query(ctx, `SELECT ps.id, ps.ledger_id, ps.fiscal_year, ps.period, ps.status, ps.opened_count
		FROM ledger.period_status ps JOIN ledger.ledger g ON g.tenant_id = ps.tenant_id AND g.id = ps.ledger_id
		WHERE ps.legal_entity_id = $1 AND ps.ledger_id = ANY($2) AND ps.fiscal_year <= $3
		ORDER BY ps.fiscal_year, ps.period, NOT g.is_leading, g.code FOR SHARE OF ps`, le, ids, year)
	var periods []readPeriod
	var p readPeriod
	if _, err := pgx.ForEachRow(rows, []any{&p.id, &p.ledger, &p.year, &p.period, &p.status, &p.opened}, func() error {
		periods = append(periods, p)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: carry-forward of %s from %d: lock the periods: %w", le, year, err)
	}
	return periods, nil
}

// carryForwardReady refuses a carry-forward a ledger is not ready for
// (ADR-0059 §5): the year not set up in it, the next year not set up, or a
// period of the year or an earlier year still open, naming them.
func carryForwardReady(ctx context.Context, tx pgx.Tx, r carryForwardRequest, ledgers []keptLedger, periods []readPeriod) error {
	var open []string
	for _, l := range ledgers {
		has := false
		for _, p := range periods {
			if p.ledger != l.id {
				continue
			}
			has = has || p.year == r.year
			if p.status == Open {
				open = append(open, fmt.Sprintf("%d/%d in %s", p.year, p.period, l.code))
			}
		}
		if !has {
			return problem.Unprocessable("ledger.no_fiscal_period", fmt.Sprintf("Fiscal year %d is not set up in ledger %s of company %s.", r.year, l.code, r.entity.Code),
				problem.FieldError{Field: "fiscal_year", Code: "no_fiscal_period", Message: "a fiscal year set up in every ledger named"})
		}
	}
	if err := nextYearSetUp(ctx, tx, r, ledgers); err != nil {
		return err
	}
	if len(open) > 0 {
		return problem.Conflict("ledger.carry_forward_periods_open", "Periods of "+fmt.Sprint(r.year)+" or earlier are still open: "+strings.Join(open, ", ")+
			"; close them first, so nothing is posted into what is carried.")
	}
	return nil
}

// nextYearSetUp refuses a carry-forward into a year a ledger has no
// periods of: the carry-forward is the opening of a year the company keeps.
func nextYearSetUp(ctx context.Context, tx pgx.Tx, r carryForwardRequest, ledgers []keptLedger) error {
	for _, l := range ledgers {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.period_status WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3)`,
			r.entity.ID, l.id, r.year+1).Scan(&ok); err != nil {
			return fmt.Errorf("ledger: carry-forward of %s from %d: next year in %s: %w", r.entity.ID, r.year, l.code, err)
		}
		if !ok {
			return problem.Conflict("ledger.next_year_not_set_up", fmt.Sprintf("Fiscal year %d is not set up in ledger %s of company %s; "+
				"set it up with a number range for it before carrying %d forward into it.", r.year+1, l.code, r.entity.Code, r.year))
		}
	}
	return nil
}

// carryForward runs the carry-forward of one ledger: chained from the
// previous year's current run when it has one that is not stale, from
// every year when it has none; the year's current run, if any, superseded
// by the new one. Audited and published as BalanceCarriedForward.
func carryForward(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r carryForwardRequest, l keptLedger, periods []readPeriod) (CarryForwardRun, error) {
	run := CarryForwardRun{ID: uuid.Must(uuid.NewV7()), LegalEntityID: r.entity.ID, LedgerID: l.id, FromYear: r.year, ToYear: r.year + 1,
		Basis: BasisAllYears, RetainedEarnings: r.retainedEarnings}
	prev, err := currentRun(ctx, tx, r.entity.ID, l.id, r.year-1)
	if err != nil {
		return run, err
	}
	if prev != nil {
		if err := notStale(ctx, tx, *prev, l); err != nil {
			return run, err
		}
		run.Basis, run.ChainedFromID = BasisChained, &prev.ID
	}
	rows, err := carriedRows(ctx, tx, run)
	if err != nil {
		return run, err
	}
	superseded, err := currentRun(ctx, tx, r.entity.ID, l.id, r.year)
	if err != nil {
		return run, err
	}
	if superseded != nil {
		if err := supersede(ctx, tx, superseded.ID, run.ID); err != nil {
			return run, err
		}
	}
	if err := recordCarryForward(ctx, tx, p, run, rows, periods); err != nil {
		return run, err
	}
	stored, err := getCarryForward(ctx, tx, run.ID)
	if err != nil {
		return run, err
	}
	return stored, carriedForward(ctx, tx, r, stored, superseded)
}

// notStale refuses chaining from a run a period it read has been opened
// since, or a period of its years it did not read: its figures may have
// changed, so the previous year is carried forward again first.
func notStale(ctx context.Context, tx pgx.Tx, prev CarryForwardRun, l keptLedger) error {
	opened, err := openedSince(ctx, tx, prev)
	if err != nil || len(opened) == 0 {
		return err
	}
	names := make([]string, len(opened))
	for i, o := range opened {
		names[i] = fmt.Sprintf("%d/%d", o.FiscalYear, o.Period)
	}
	return problem.Conflict("ledger.carry_forward_stale", fmt.Sprintf("The carry-forward of %d in ledger %s is stale: periods %s were opened since it ran; "+
		"carry %d forward again first.", prev.FromYear, l.code, strings.Join(names, ", "), prev.FromYear))
}

// supersede marks the current run of the year superseded by the new run,
// which is inserted next in the same transaction (the database checks the
// successor at commit).
func supersede(ctx context.Context, tx pgx.Tx, id, by uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE ledger.carry_forward_run SET status = 'superseded', superseded_by_id = $2, superseded_at = now(), version = version + 1
		WHERE id = $1 AND status = 'current'`, id, by); err != nil {
		return fmt.Errorf("ledger: supersede carry-forward run %s: %w", id, err)
	}
	return nil
}

// carriedRow is one opening balance of a run.
type carriedRow struct {
	account    uuid.UUID
	kind       string
	currency   string
	tc, lc, gc money.Minor
}

// carriedRows computes a run's rows (ADR-0059 §5) from the previous
// year's run's rows when chained, whose result rows fold into the retained
// earnings account's balance as it is a balance sheet account, and the
// ledger's balances of the year (chained) or of every year up to it, as
// the trial balance reads them: projected balances plus the lines the
// projector has not reached. A balance sheet account carries its own; a
// profit and loss account's year goes to the retained earnings account,
// the run's year as its result, earlier years into its balance. A row whose
// amounts are all zero is left out.
func carriedRows(ctx context.Context, tx pgx.Tx, run CarryForwardRun) ([]carriedRow, error) {
	var from *int16
	if run.ChainedFromID != nil {
		from = &run.FromYear
	}
	rows, _ := tx.Query(ctx, `WITH src AS (
			SELECT tenant_id, account_id, currency_tc, amount_tc AS tc, amount_lc AS lc, amount_gc AS gc, false AS this_year
			FROM ledger.carry_forward_balance WHERE run_id = $5
			UNION ALL
			SELECT tenant_id, account_id, currency_tc, debit_tc - credit_tc, debit_lc - credit_lc, debit_gc - credit_gc, fiscal_year = $3
			FROM ledger.period_balance
			WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year <= $3 AND ($4::smallint IS NULL OR fiscal_year >= $4)
			UNION ALL
			SELECT l.tenant_id, l.account_id, e.currency_tc, l.amount_tc, l.amount_lc, l.amount_gc, l.fiscal_year = $3
			FROM ledger.balance_projection q
			JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
			JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
			WHERE q.projected_at IS NULL AND q.fiscal_year <= $3 AND ($4::smallint IS NULL OR q.fiscal_year >= $4)
				AND e.legal_entity_id = $1 AND e.ledger_id = $2
		)
		SELECT CASE WHEN a.is_balance_sheet THEN s.account_id ELSE $6 END,
			CASE WHEN NOT a.is_balance_sheet AND s.this_year THEN 'result' ELSE 'balance' END,
			s.currency_tc, sum(s.tc)::bigint, sum(s.lc)::bigint, sum(s.gc)::bigint
		FROM src s JOIN ledger.gl_account a ON a.tenant_id = s.tenant_id AND a.id = s.account_id
		GROUP BY 1, 2, 3
		HAVING sum(s.tc) <> 0 OR sum(s.lc) <> 0 OR sum(s.gc) <> 0`,
		run.LegalEntityID, run.LedgerID, run.FromYear, from, run.ChainedFromID, run.RetainedEarnings.ID)
	var out []carriedRow
	var c carriedRow
	if _, err := pgx.ForEachRow(rows, []any{&c.account, &c.kind, &c.currency, &c.tc, &c.lc, &c.gc}, func() error {
		out = append(out, c)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: carry-forward of %s from %d: balances: %w", run.LegalEntityID, run.FromYear, err)
	}
	return out, nil
}

// recordCarryForward inserts the run, its rows and the opened count of
// every period of its ledger it read.
func recordCarryForward(ctx context.Context, tx pgx.Tx, p reqctx.Principal, run CarryForwardRun, rows []carriedRow, periods []readPeriod) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
			chained_from_id, retained_earnings_account_id, row_count, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		p.TenantID, run.ID, run.LegalEntityID, run.LedgerID, run.FromYear, run.ToYear, run.Basis, run.ChainedFromID, run.RetainedEarnings.ID,
		len(rows), p.ID); err != nil {
		return fmt.Errorf("ledger: record carry-forward run %s: %w", run.ID, err)
	}
	n := len(rows)
	accounts, kinds, currencies := make([]uuid.UUID, n), make([]string, n), make([]string, n)
	tcs, lcs, gcs := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n)
	for i, r := range rows {
		accounts[i], kinds[i], currencies[i], tcs[i], lcs[i], gcs[i] = r.account, r.kind, r.currency, r.tc, r.lc, r.gc
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.carry_forward_balance (tenant_id, run_id, account_id, kind, currency_tc, amount_tc, amount_lc, amount_gc)
		SELECT $1, $2, a, k, c, tc, lc, gc FROM unnest($3::uuid[], $4::text[], $5::text[], $6::bigint[], $7::bigint[], $8::bigint[]) AS x(a, k, c, tc, lc, gc)`,
		p.TenantID, run.ID, accounts, kinds, currencies, tcs, lcs, gcs); err != nil {
		return fmt.Errorf("ledger: record carry-forward run %s: balances: %w", run.ID, err)
	}
	var ids []uuid.UUID
	var counts []int32
	for _, pd := range periods {
		if pd.ledger == run.LedgerID {
			ids, counts = append(ids, pd.id), append(counts, pd.opened)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.carry_forward_period (tenant_id, run_id, period_id, opened_count)
		SELECT $1, $2, i, c FROM unnest($3::uuid[], $4::int[]) AS x(i, c)`, p.TenantID, run.ID, ids, counts); err != nil {
		return fmt.Errorf("ledger: record carry-forward run %s: periods: %w", run.ID, err)
	}
	return nil
}

// carriedForward audits a run, with the run it superseded, and publishes
// BalanceCarriedForward with the year's result per currency.
func carriedForward(ctx context.Context, tx pgx.Tx, r carryForwardRequest, run CarryForwardRun, superseded *CarryForwardRun) error {
	totals, err := carryForwardTotals(ctx, tx, run.ID, r.entity.LocalCurrency, r.group)
	if err != nil {
		return err
	}
	results := []map[string]any{}
	for _, t := range totals {
		if t.hasResult {
			results = append(results, map[string]any{"currency": t.currency, "result": money.EventAmount(t.result.tc, t.currency, t.expTC),
				"result_lc": money.EventAmount(t.result.lc, t.currencyLC, t.expLC), "result_gc": money.EventAmount(t.result.gc, t.currencyGC, t.expGC)})
		}
	}
	all, err := carryForwardRunTotals(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	exps, err := exponents(ctx, tx, r.entity.LocalCurrency, r.group)
	if err != nil {
		return err
	}
	var supersededID *uuid.UUID
	if superseded != nil {
		supersededID = &superseded.ID
	}
	return audit.Record(ctx, "ledger.carry_forward_run.run", "ledger.carry_forward_run", run.ID,
		map[string]any{"from_fiscal_year": run.FromYear, "to_fiscal_year": run.ToYear, "ledger_id": run.LedgerID, "basis": run.Basis,
			"chained_from_id": run.ChainedFromID, "superseded_run_id": supersededID, "retained_earnings_account_id": run.RetainedEarnings.ID, "row_count": run.RowCount},
		outbox.Event{Context: "ledger", Name: "balance_carried_forward", AggregateType: "ledger.carry_forward_run", AggregateID: run.ID,
			Payload: map[string]any{"run_id": run.ID, "legal_entity_id": run.LegalEntityID, "ledger_id": run.LedgerID, "from_fiscal_year": run.FromYear,
				"to_fiscal_year": run.ToYear, "basis": run.Basis, "chained_from_id": run.ChainedFromID, "superseded_run_id": supersededID,
				"retained_earnings_account_id": run.RetainedEarnings.ID, "row_count": run.RowCount, "result_lc": money.EventAmount(all.result.lc, r.entity.LocalCurrency, exps[r.entity.LocalCurrency]),
				"result_gc": money.EventAmount(all.result.gc, r.group, exps[r.group]), "results": results}})
}
