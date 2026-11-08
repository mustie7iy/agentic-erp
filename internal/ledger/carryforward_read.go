package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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

// openedSince are the periods that make a run stale (ADR-0060 §6): of the
// periods of its ledger in its year and every earlier year, those open now
// or opened since the run read them, and those it did not read (set up
// later) that are open or have been opened. A posting needs an open
// period, so none of the others has taken one since the run.
func openedSince(ctx context.Context, tx pgx.Tx, run CarryForwardRun) ([]PeriodRef, error) {
	rows, _ := tx.Query(ctx, `SELECT ps.ledger_id, ps.fiscal_year, ps.period FROM ledger.period_status ps
		LEFT JOIN ledger.carry_forward_period cp ON cp.tenant_id = ps.tenant_id AND cp.run_id = $1 AND cp.period_id = ps.id
		WHERE ps.legal_entity_id = $2 AND ps.ledger_id = $3 AND ps.fiscal_year <= $4
			AND (ps.status = 'open' OR ps.opened_count <> coalesce(cp.opened_count, 0))
		ORDER BY ps.fiscal_year, ps.period`, run.ID, run.LegalEntityID, run.LedgerID, run.FromYear)
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PeriodRef, error) {
		var p PeriodRef
		return p, errs.Wrapf(row.Scan(&p.LedgerID, &p.FiscalYear, &p.Period), "scan period opened since")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: periods opened since carry-forward run %s: %w", run.ID, err)
	}
	return refs, nil
}

// amounts are an amount in transaction, local and group currency.
type amounts struct{ tc, lc, gc money.Minor }

// carryForwardTotal is a run's figures in one transaction currency: its
// result row, if any, and the debit and credit totals of its rows.
type carryForwardTotal struct {
	currency, currencyLC, currencyGC string
	expTC, expLC, expGC              int
	hasResult                        bool
	result, debit, credit            amounts
}

// runTotals are a run's figures over every transaction currency, in local
// and group currency (the tc amounts are not summed: they are in different
// currencies).
type runTotals struct {
	result, debit, credit amounts
}

// carryForwardRunTotals sums all of a run's rows in local and group
// currency: the year's result and the debits and credits.
func carryForwardRunTotals(ctx context.Context, tx pgx.Tx, run uuid.UUID) (runTotals, error) {
	var t runTotals
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_lc) FILTER (WHERE kind = 'result'), 0)::bigint, coalesce(sum(amount_gc) FILTER (WHERE kind = 'result'), 0)::bigint,
			coalesce(sum(greatest(amount_lc, 0)), 0)::bigint, coalesce(sum(greatest(-amount_lc, 0)), 0)::bigint,
			coalesce(sum(greatest(amount_gc, 0)), 0)::bigint, coalesce(sum(greatest(-amount_gc, 0)), 0)::bigint
		FROM ledger.carry_forward_balance WHERE run_id = $1`, run).Scan(&t.result.lc, &t.result.gc, &t.debit.lc, &t.credit.lc, &t.debit.gc, &t.credit.gc)
	return t, errs.Wrapf(err, "ledger: totals of carry-forward run %s in local and group currency", run)
}

// carryForwardTotals sums a run's rows per transaction currency, by code;
// lc and gc are the run's local and group currencies.
func carryForwardTotals(ctx context.Context, tx pgx.Tx, run uuid.UUID, lc, gc string) ([]carryForwardTotal, error) {
	exps, err := exponents(ctx, tx, lc, gc)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT b.currency_tc, c.exponent, bool_or(b.kind = 'result'),
			coalesce(sum(b.amount_tc) FILTER (WHERE b.kind = 'result'), 0)::bigint, coalesce(sum(b.amount_lc) FILTER (WHERE b.kind = 'result'), 0)::bigint,
			coalesce(sum(b.amount_gc) FILTER (WHERE b.kind = 'result'), 0)::bigint,
			sum(greatest(b.amount_tc, 0))::bigint, sum(greatest(-b.amount_tc, 0))::bigint, sum(greatest(b.amount_lc, 0))::bigint,
			sum(greatest(-b.amount_lc, 0))::bigint, sum(greatest(b.amount_gc, 0))::bigint, sum(greatest(-b.amount_gc, 0))::bigint
		FROM ledger.carry_forward_balance b JOIN ref.currency c ON c.code = b.currency_tc
		WHERE b.run_id = $1 GROUP BY 1, 2 ORDER BY 1`, run)
	var out []carryForwardTotal
	t := carryForwardTotal{currencyLC: lc, currencyGC: gc, expLC: exps[lc], expGC: exps[gc]}
	if _, err := pgx.ForEachRow(rows, []any{&t.currency, &t.expTC, &t.hasResult, &t.result.tc, &t.result.lc, &t.result.gc,
		&t.debit.tc, &t.credit.tc, &t.debit.lc, &t.credit.lc, &t.debit.gc, &t.credit.gc}, func() error {
		out = append(out, t)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: totals of carry-forward run %s: %w", run, err)
	}
	return out, nil
}

// GetBalanceCarryForward implements GET /v1/balance-carry-forwards/{id}.
func (e *Endpoints) GetBalanceCarryForward(ctx context.Context, params gen.GetBalanceCarryForwardParams) (gen.GetBalanceCarryForwardRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	run, err := getCarryForward(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	rep, err := e.carryForwardToAPI(ctx, tx, run)
	if err != nil {
		return nil, err
	}
	return &gen.BalanceCarryForwardHeaders{ETag: gen.NewOptString(etag.Format(run.Version)), Response: rep}, nil
}

// carryForwardToAPI renders a run with its company, who ran it and its
// totals.
func (e *Endpoints) carryForwardToAPI(ctx context.Context, tx pgx.Tx, run CarryForwardRun) (gen.BalanceCarryForward, error) {
	// A run is recorded for an existing legal entity, which is never deleted.
	le, _, err := e.LegalEntities.Entity(ctx, run.LegalEntityID)
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	names, err := idref.Resolve(ctx, e.PersonNames.PersonNames, []uuid.UUID{run.CreatedBy}, idref.Kind{Name: "person", Context: "identity"})
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	totals, err := carryForwardTotals(ctx, tx, run.ID, le.LocalCurrency, group)
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	all, err := carryForwardRunTotals(ctx, tx, run.ID)
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	exps, err := exponents(ctx, tx, le.LocalCurrency, group)
	if err != nil {
		return gen.BalanceCarryForward{}, err
	}
	lc := func(m money.Minor) gen.Money { return apitypes.Money(m, le.LocalCurrency, exps[le.LocalCurrency]) }
	gc := func(m money.Minor) gen.Money { return apitypes.Money(m, group, exps[group]) }
	out := gen.BalanceCarryForward{ID: run.ID, LegalEntityID: run.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, LedgerID: run.LedgerID,
		LedgerCode: run.LedgerCode, LedgerName: run.LedgerName, FromFiscalYear: int32(run.FromYear), ToFiscalYear: int32(run.ToYear), Basis: run.Basis,
		ChainedFromID: apitypes.OptUUID(run.ChainedFromID), RetainedEarningsAccount: gen.CarryForwardAccount{ID: run.RetainedEarnings.ID,
			Number: run.RetainedEarnings.Number, Name: run.RetainedEarnings.Name}, Status: run.Status, SupersededByID: apitypes.OptUUID(run.SupersededByID),
		RowCount: run.RowCount, LocalCurrency: le.LocalCurrency, GroupCurrency: group, Totals: make([]gen.CarryForwardTotal, len(totals)),
		ResultLc: lc(all.result.lc), ResultGc: gc(all.result.gc), DebitLc: lc(all.debit.lc), CreditLc: lc(all.credit.lc), DebitGc: gc(all.debit.gc),
		CreditGc: gc(all.credit.gc), CreatedAt: run.CreatedAt, CreatedBy: run.CreatedBy, CreatedByName: names[run.CreatedBy]}
	if run.SupersededAt != nil {
		out.SupersededAt.SetTo(*run.SupersededAt)
	}
	for i, t := range totals {
		tc := func(m money.Minor) gen.Money { return apitypes.Money(m, t.currency, t.expTC) }
		out.Totals[i] = gen.CarryForwardTotal{Currency: t.currency, Result: tc(t.result.tc), ResultLc: lc(t.result.lc), ResultGc: gc(t.result.gc),
			Debit: tc(t.debit.tc), Credit: tc(t.credit.tc), DebitLc: lc(t.debit.lc), CreditLc: lc(t.credit.lc), DebitGc: gc(t.debit.gc), CreditGc: gc(t.credit.gc)}
	}
	return out, nil
}
