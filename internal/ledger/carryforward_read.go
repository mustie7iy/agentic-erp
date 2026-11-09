package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// CarryForwardRun is a balance carry-forward run of a company and ledger
// from one fiscal year into the next (ADR-0060 §5).
type CarryForwardRun struct {
	ID, LegalEntityID, LedgerID uuid.UUID
	LedgerCode, LedgerName      string
	FromYear, ToYear            int16
	Basis                       string
	ChainedFromID               *uuid.UUID
	RetainedEarnings            AccountRef
	Status                      string
	SupersededByID              *uuid.UUID
	SupersededAt                *time.Time
	RowCount                    int32
	CreatedAt                   time.Time
	CreatedBy                   uuid.UUID
	Version                     int32
}

// PeriodRef names a posting period of a ledger.
type PeriodRef struct {
	LedgerID           uuid.UUID
	FiscalYear, Period int16
}

const carryForwardSelect = `SELECT r.id, r.legal_entity_id, r.ledger_id, g.code, g.name, r.from_fiscal_year, r.to_fiscal_year, r.basis, r.chained_from_id,
		r.retained_earnings_account_id, a.number, a.name, r.status, r.superseded_by_id, r.superseded_at, r.row_count, r.created_at, r.created_by, r.version
	FROM ledger.carry_forward_run r
	JOIN ledger.ledger g ON g.tenant_id = r.tenant_id AND g.id = r.ledger_id
	JOIN ledger.gl_account a ON a.tenant_id = r.tenant_id AND a.id = r.retained_earnings_account_id`

func scanCarryForward(row pgx.Row) (CarryForwardRun, error) {
	var r CarryForwardRun
	err := errs.Wrapf(row.Scan(&r.ID, &r.LegalEntityID, &r.LedgerID, &r.LedgerCode, &r.LedgerName, &r.FromYear, &r.ToYear, &r.Basis, &r.ChainedFromID,
		&r.RetainedEarnings.ID, &r.RetainedEarnings.Number, &r.RetainedEarnings.Name, &r.Status, &r.SupersededByID, &r.SupersededAt, &r.RowCount,
		&r.CreatedAt, &r.CreatedBy, &r.Version), "scan carry-forward run")
	return r, err
}

// currentRun is the current run of a company, ledger and year, nil when
// it has none.
func currentRun(ctx context.Context, tx pgx.Tx, le, ledgerID uuid.UUID, year int16) (*CarryForwardRun, error) {
	r, err := scanCarryForward(tx.QueryRow(ctx, carryForwardSelect+` WHERE r.legal_entity_id = $1 AND r.ledger_id = $2 AND r.from_fiscal_year = $3
		AND r.status = 'current'`, le, ledgerID, year))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: current carry-forward of %s from %d: %w", le, year, err)
	}
	return &r, nil
}

// getCarryForward loads a run of a legal entity the caller may act in.
func getCarryForward(ctx context.Context, tx pgx.Tx, id uuid.UUID) (CarryForwardRun, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return CarryForwardRun{}, err
	}
	r, err := scanCarryForward(tx.QueryRow(ctx, carryForwardSelect+` WHERE r.id = $1`, id))
	if err = authz.Visible(err, access, r.LegalEntityID, problem.NotFoundOf("carry-forward run"), "ledger: read carry-forward run %s", id); err != nil {
		return CarryForwardRun{}, err
	}
	return r, nil
}

// openedSinceOf are the periods that make each of the runs stale
// (ADR-0060 §6), by run: of the periods of its ledger in its year and
// every earlier year, those open now or opened since the run read them,
// and those it did not read (set up later) that are open or have been
// opened. A posting needs an open period, so none of the others has taken
// one since the run. A run that is not stale has none.
func openedSinceOf(ctx context.Context, tx pgx.Tx, runs []uuid.UUID) (map[uuid.UUID][]PeriodRef, error) {
	rows, _ := tx.Query(ctx, `SELECT r.id, ps.ledger_id, ps.fiscal_year, ps.period FROM ledger.carry_forward_run r
		JOIN ledger.period_status ps ON ps.tenant_id = r.tenant_id AND ps.legal_entity_id = r.legal_entity_id AND ps.ledger_id = r.ledger_id
			AND ps.fiscal_year <= r.from_fiscal_year
		LEFT JOIN ledger.carry_forward_period cp ON cp.tenant_id = ps.tenant_id AND cp.run_id = r.id AND cp.period_id = ps.id
		WHERE r.id = ANY($1) AND (ps.status = 'open' OR ps.opened_count <> coalesce(cp.opened_count, 0))
		ORDER BY r.id, ps.fiscal_year, ps.period`, runs)
	out := map[uuid.UUID][]PeriodRef{}
	var run uuid.UUID
	var p PeriodRef
	if _, err := pgx.ForEachRow(rows, []any{&run, &p.LedgerID, &p.FiscalYear, &p.Period}, func() error {
		out[run] = append(out[run], p)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: periods opened since carry-forward runs %v: %w", runs, err)
	}
	return out, nil
}

// amounts are an amount in transaction, local and group currency.
type amounts struct{ tc, lc, gc money.Minor }

// carryForwardTotal is a run's figures in one transaction currency: its
// result row, if any, and the debit and credit totals of its rows.
type carryForwardTotal struct {
	currency              string
	expTC                 int
	hasResult             bool
	result, debit, credit amounts
}

// runTotals are a run's figures over every transaction currency, in local
// and group currency (the tc amounts are not summed: they are in different
// currencies).
type runTotals struct {
	result, debit, credit amounts
}

// overall sums a run's totals per transaction currency in local and group
// currency: every row is in exactly one of them, so the sums are the
// run's.
func overall(totals []carryForwardTotal) runTotals {
	var r runTotals
	for _, t := range totals {
		r.result.lc, r.result.gc = r.result.lc+t.result.lc, r.result.gc+t.result.gc
		r.debit.lc, r.debit.gc = r.debit.lc+t.debit.lc, r.debit.gc+t.debit.gc
		r.credit.lc, r.credit.gc = r.credit.lc+t.credit.lc, r.credit.gc+t.credit.gc
	}
	return r
}

// carryForwardTotalsOf sums each run's rows per transaction currency, by
// run and currency code.
func carryForwardTotalsOf(ctx context.Context, tx pgx.Tx, runs []uuid.UUID) (map[uuid.UUID][]carryForwardTotal, error) {
	rows, _ := tx.Query(ctx, `SELECT b.run_id, b.currency_tc, c.exponent, bool_or(b.kind = 'result'),
			coalesce(sum(b.amount_tc) FILTER (WHERE b.kind = 'result'), 0)::bigint, coalesce(sum(b.amount_lc) FILTER (WHERE b.kind = 'result'), 0)::bigint,
			coalesce(sum(b.amount_gc) FILTER (WHERE b.kind = 'result'), 0)::bigint,
			sum(greatest(b.amount_tc, 0))::bigint, sum(greatest(-b.amount_tc, 0))::bigint, sum(greatest(b.amount_lc, 0))::bigint,
			sum(greatest(-b.amount_lc, 0))::bigint, sum(greatest(b.amount_gc, 0))::bigint, sum(greatest(-b.amount_gc, 0))::bigint
		FROM ledger.carry_forward_balance b JOIN ref.currency c ON c.code = b.currency_tc
		WHERE b.run_id = ANY($1) GROUP BY 1, 2, 3 ORDER BY 1, 2`, runs)
	out := map[uuid.UUID][]carryForwardTotal{}
	var run uuid.UUID
	var t carryForwardTotal
	if _, err := pgx.ForEachRow(rows, []any{&run, &t.currency, &t.expTC, &t.hasResult, &t.result.tc, &t.result.lc, &t.result.gc,
		&t.debit.tc, &t.credit.tc, &t.debit.lc, &t.credit.lc, &t.debit.gc, &t.credit.gc}, func() error {
		out[run] = append(out[run], t)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: totals of carry-forward runs %v: %w", runs, err)
	}
	return out, nil
}

// GetBalanceCarryForward implements GET /v1/balance-carry-forwards/{id}.
// Its ETag is the representation's: stale moves without the run's version.
func (e *Endpoints) GetBalanceCarryForward(ctx context.Context, params gen.GetBalanceCarryForwardParams) (gen.GetBalanceCarryForwardRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run, err := getCarryForward(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	reps, err := e.carryForwardsToAPI(ctx, tx, []CarryForwardRun{run})
	if err != nil {
		return nil, err
	}
	return &gen.BalanceCarryForwardHeaders{ETag: apitypes.ETagOf(reps[0]), Response: reps[0]}, nil
}

var carryForwardSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"}, {Name: "fiscal_year", Column: "from_fiscal_year", Type: "smallint"},
}}

// ListBalanceCarryForwards implements GET /v1/balance-carry-forwards: the
// runs of the legal entities the caller may act in, filtered, each shown
// as GET shows it.
func (e *Endpoints) ListBalanceCarryForwards(ctx context.Context, params gen.ListBalanceCarryForwardsParams) (gen.ListBalanceCarryForwardsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ledgerID := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.LedgerID)
	var year *int16
	if params.FiscalYear.Set {
		y := int16(params.FiscalYear.Value)
		year = &y
	}
	// The filter's values, never a pointer's address, bind the page tokens.
	scope := fmt.Sprintf("listBalanceCarryForwards|%v|%t %d|%v|%s", le, params.FiscalYear.Set, params.FiscalYear.Value, ledgerID, params.Status.Value)
	ks, err := e.Pages.Keyset(carryForwardSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	// The runs' columns, wrapped so the page's order and cursor name them
	// alone (the joined ledger and account have ids and names too).
	q, args := ks.SQL(`SELECT * FROM (`+carryForwardSelect+` WHERE ($1 OR r.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR r.legal_entity_id = $3)
			AND ($4::smallint IS NULL OR r.from_fiscal_year = $4) AND ($5::uuid IS NULL OR r.ledger_id = $5) AND ($6 = '' OR r.status = $6)) r WHERE true`,
		[]any{access.All, access.IDs, le, year, ledgerID, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CarryForwardRun, error) { return scanCarryForward(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list carry-forward runs: %w", err)
	}
	runs, next := pagination.Page(ks, runs, func(r CarryForwardRun) (string, uuid.UUID) {
		if ks.Field() == "fiscal_year" {
			return fmt.Sprint(r.FromYear), r.ID
		}
		return r.CreatedAt.Format(time.RFC3339Nano), r.ID
	})
	page := gen.BalanceCarryForwardPage{}
	if page.Items, err = e.carryForwardsToAPI(ctx, tx, runs); err != nil {
		return nil, err
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &page, nil
}

// carryForwardView is what a page of runs is shown with, read once for
// the page: the companies, the group currency and the minor units of their
// currencies, who ran them, their totals and the periods that make them
// stale.
type carryForwardView struct {
	companies map[uuid.UUID]tenancy.Entity
	group     string
	exps      map[string]int
	names     map[uuid.UUID]string
	totals    map[uuid.UUID][]carryForwardTotal
	opened    map[uuid.UUID][]PeriodRef
}

// carryForwardViewOf reads the view of the runs.
func (e *Endpoints) carryForwardViewOf(ctx context.Context, tx pgx.Tx, runs []CarryForwardRun) (carryForwardView, error) {
	v := carryForwardView{companies: map[uuid.UUID]tenancy.Entity{}}
	var err error
	if v.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return v, err
	}
	ids, people, codes := make([]uuid.UUID, len(runs)), make([]uuid.UUID, 0, len(runs)), []string{v.group}
	for i, r := range runs {
		ids[i] = r.ID
		if !slices.Contains(people, r.CreatedBy) {
			people = append(people, r.CreatedBy)
		}
		if _, ok := v.companies[r.LegalEntityID]; ok {
			continue
		}
		// A run is recorded for an existing legal entity, which is never deleted.
		le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
		if err != nil {
			return v, err
		}
		v.companies[r.LegalEntityID], codes = le, append(codes, le.LocalCurrency)
	}
	if v.exps, err = exponents(ctx, tx, codes...); err != nil {
		return v, err
	}
	if v.names, err = idref.Resolve(ctx, e.PersonNames.PersonNames, people, idref.Kind{Name: "person", Context: "identity"}); err != nil {
		return v, err
	}
	if v.totals, err = carryForwardTotalsOf(ctx, tx, ids); err != nil {
		return v, err
	}
	v.opened, err = openedSinceOf(ctx, tx, ids)
	return v, err
}

// carryForwardFigures are a run's totals as the API shows them: per
// transaction currency, and over every currency in local and group
// currency.
type carryForwardFigures struct {
	totals                                         []gen.CarryForwardTotal
	resultLC, resultGC, debitLC, creditLC, debitGC gen.Money
	creditGC                                       gen.Money
}

// figures renders a run's totals in the company's local currency lc.
func (v carryForwardView) figures(run uuid.UUID, lc string) carryForwardFigures {
	totals := v.totals[run]
	lcm := func(m money.Minor) gen.Money { return apitypes.Money(m, lc, v.exps[lc]) }
	gcm := func(m money.Minor) gen.Money { return apitypes.Money(m, v.group, v.exps[v.group]) }
	all := overall(totals)
	f := carryForwardFigures{totals: make([]gen.CarryForwardTotal, len(totals)), resultLC: lcm(all.result.lc), resultGC: gcm(all.result.gc),
		debitLC: lcm(all.debit.lc), creditLC: lcm(all.credit.lc), debitGC: gcm(all.debit.gc), creditGC: gcm(all.credit.gc)}
	for i, t := range totals {
		tc := func(m money.Minor) gen.Money { return apitypes.Money(m, t.currency, t.expTC) }
		f.totals[i] = gen.CarryForwardTotal{Currency: t.currency, Result: tc(t.result.tc), ResultLc: lcm(t.result.lc), ResultGc: gcm(t.result.gc),
			Debit: tc(t.debit.tc), Credit: tc(t.credit.tc), DebitLc: lcm(t.debit.lc), CreditLc: lcm(t.credit.lc), DebitGc: gcm(t.debit.gc), CreditGc: gcm(t.credit.gc)}
	}
	return f
}

// carryForwardsToAPI renders runs with their companies, who ran them,
// their totals and whether they are stale, judged now.
func (e *Endpoints) carryForwardsToAPI(ctx context.Context, tx pgx.Tx, runs []CarryForwardRun) ([]gen.BalanceCarryForward, error) {
	v, err := e.carryForwardViewOf(ctx, tx, runs)
	if err != nil {
		return nil, err
	}
	out := make([]gen.BalanceCarryForward, len(runs))
	for i, run := range runs {
		le := v.companies[run.LegalEntityID]
		f := v.figures(run.ID, le.LocalCurrency)
		rep := gen.BalanceCarryForward{ID: run.ID, LegalEntityID: run.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, LedgerID: run.LedgerID,
			LedgerCode: run.LedgerCode, LedgerName: run.LedgerName, FromFiscalYear: int32(run.FromYear), ToFiscalYear: int32(run.ToYear), Basis: run.Basis,
			ChainedFromID: apitypes.OptUUID(run.ChainedFromID), RetainedEarningsAccount: gen.CarryForwardAccount{ID: run.RetainedEarnings.ID,
				Number: run.RetainedEarnings.Number, Name: run.RetainedEarnings.Name}, Status: run.Status, SupersededByID: apitypes.OptUUID(run.SupersededByID),
			RowCount: run.RowCount, LocalCurrency: le.LocalCurrency, GroupCurrency: v.group, Totals: f.totals, ResultLc: f.resultLC, ResultGc: f.resultGC,
			DebitLc: f.debitLC, CreditLc: f.creditLC, DebitGc: f.debitGC, CreditGc: f.creditGC, CreatedAt: run.CreatedAt, CreatedBy: run.CreatedBy,
			CreatedByName: v.names[run.CreatedBy], OpenedSince: make([]gen.CarryForwardOpenedPeriod, len(v.opened[run.ID]))}
		if run.SupersededAt != nil {
			rep.SupersededAt.SetTo(*run.SupersededAt)
		}
		for j, p := range v.opened[run.ID] {
			rep.OpenedSince[j] = gen.CarryForwardOpenedPeriod{LedgerID: p.LedgerID, LedgerCode: run.LedgerCode, FiscalYear: int32(p.FiscalYear), Period: int32(p.Period)}
		}
		rep.Stale = len(rep.OpenedSince) > 0
		out[i] = rep
	}
	return out, nil
}
