package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ConsolidationReads is what the ledger publishes to the consolidation
// context (ADR-0059 §4a, T68d): the leading ledger, a company's posting
// period with its status and version, its accounts, its balances through a
// period per account and per account and trading partner, and a rate of a
// type valid on a date. Each runs in the request's or activity's
// transaction under row-level security (ADR-0003); consolidation reads no
// ledger table itself, and the composition root adapts these to its
// Ledger port.
type ConsolidationReads struct{}

// PeriodState is a company's posting period in a ledger: its id, dates,
// status and version, which moves with every change of the period (a
// close, a lock, an unlock request and its decision), so a run that
// recorded it sees any change since (ADR-0059 §10).
type PeriodState struct {
	ID                 uuid.UUID
	FiscalYear, Period int16
	Start, End         time.Time
	Status             string
	Version            int32
}

// AccountOf is a company's account: its id, number, name and type.
type AccountOf struct {
	ID           uuid.UUID
	Number, Name string
	Type         string
}

// BalanceThrough is a company's balance of one account through a period,
// in local currency and at its booked group amounts, of the lines naming
// TradingPartnerID, or of those naming none when it is nil. A balance
// sheet account's is over every fiscal year through the period, with
// FiscalYear the period's year and Period 0; a profit and loss account's
// is per fiscal year before the period's year (Period 0) and per period
// of the period's year through it.
type BalanceThrough struct {
	AccountID          uuid.UUID
	TradingPartnerID   *uuid.UUID
	FiscalYear, Period int16
	LC, GC             money.Minor
}

// RateValid is a rate of a type from one currency to another valid on a
// date: its 12-decimal form, as a journal entry records a rate, exact
// when the rate is recorded in that direction and the reciprocal's
// rounding otherwise, and the date it is valid from.
type RateValid struct {
	Rate      *big.Rat
	Shown     string
	ValidFrom time.Time
}

// LeadingLedger is the tenant's leading ledger (ADR-0045), which
// consolidation reads alone (ADR-0059 §1).
func (ConsolidationReads) LeadingLedger(ctx context.Context) (uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE is_leading`).Scan(&id)
	return id, errs.Wrapf(err, "ledger: the leading ledger")
}

// PeriodOf is the company's period of the fiscal year in the ledger, with
// its status and version; found is false when the company has none.
func (ConsolidationReads) PeriodOf(ctx context.Context, legalEntity, ledgerID uuid.UUID, year, period int16) (PeriodState, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PeriodState{}, false, err
	}
	var p PeriodState
	err = tx.QueryRow(ctx, `SELECT ps.id, ps.fiscal_year, ps.period, fp.start_date, fp.end_date, ps.status, ps.version
		FROM ledger.period_status ps
		JOIN ledger.fiscal_period fp ON fp.tenant_id = ps.tenant_id AND fp.calendar_id = ps.calendar_id AND fp.fiscal_year = ps.fiscal_year
			AND fp.period = ps.period
		WHERE ps.legal_entity_id = $1 AND ps.ledger_id = $2 AND ps.fiscal_year = $3 AND ps.period = $4`, legalEntity, ledgerID, year, period).
		Scan(&p.ID, &p.FiscalYear, &p.Period, &p.Start, &p.End, &p.Status, &p.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return PeriodState{}, false, nil
	}
	if err != nil {
		return PeriodState{}, false, fmt.Errorf("ledger: period %d/%d of %s: %w", year, period, legalEntity, err)
	}
	return p, true, nil
}

// AccountsOf are the company's accounts by number.
func (ConsolidationReads) AccountsOf(ctx context.Context, legalEntity uuid.UUID) ([]AccountOf, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT a.id, a.number, a.name, a.account_type FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id
		WHERE ca.legal_entity_id = $1 ORDER BY a.number`, legalEntity)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AccountOf, error) {
		var a AccountOf
		return a, r.Scan(&a.ID, &a.Number, &a.Name, &a.Type)
	})
	return out, errs.Wrapf(err, "ledger: accounts of %s", legalEntity)
}

// balancesThrough reads a company's balances in a ledger through a period
// ($1 legal entity, $2 ledger, $3 fiscal year, $4 period) in one
// statement, so the projected balances and the lines not projected yet are
// of one database state, exactly as balanceRows reads them: the projected
// period and trading partner balances plus the pending lines. Each figure
// is counted once per account (trading partner none) and, when it has
// one, once more under its trading partner and taken off the account's,
// so the rows without a trading partner are what the lines naming none
// hold. Balance sheet accounts sum over every fiscal year through the
// period; profit and loss accounts per earlier fiscal year and per period
// of the period's year.
const balancesThrough = `WITH f AS (
		SELECT account_id, NULL::uuid AS tp, 1 AS sign, fiscal_year, fiscal_period, debit_lc - credit_lc AS lc, debit_gc - credit_gc AS gc
		FROM ledger.period_balance
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND (fiscal_year < $3 OR (fiscal_year = $3 AND fiscal_period <= $4))
		UNION ALL
		SELECT account_id, trading_partner_id, 0, fiscal_year, fiscal_period, debit_lc - credit_lc, debit_gc - credit_gc
		FROM ledger.trading_partner_balance
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND (fiscal_year < $3 OR (fiscal_year = $3 AND fiscal_period <= $4))
		UNION ALL
		SELECT l.account_id, l.trading_partner_id, 2, l.fiscal_year, l.fiscal_period, l.amount_lc, l.amount_gc
		FROM ledger.balance_projection q
		JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
		JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
		WHERE q.projected_at IS NULL AND q.fiscal_year <= $3 AND e.legal_entity_id = $1 AND e.ledger_id = $2
			AND (l.fiscal_year < $3 OR l.fiscal_period <= $4)
	), x AS (
		-- sign 1: an account's total; 0: a trading partner's, taken off the
		-- total; 2: a pending line, in the total and under its partner.
		SELECT account_id, NULL::uuid AS tp, fiscal_year, fiscal_period, lc, gc FROM f WHERE sign IN (1, 2)
		UNION ALL
		SELECT account_id, tp, fiscal_year, fiscal_period, lc, gc FROM f WHERE sign IN (0, 2) AND tp IS NOT NULL
		UNION ALL
		SELECT account_id, NULL, fiscal_year, fiscal_period, -lc, -gc FROM f WHERE sign IN (0, 2) AND tp IS NOT NULL
	)
	SELECT x.account_id, x.tp,
		(CASE WHEN a.is_balance_sheet THEN $3 ELSE x.fiscal_year END)::smallint AS year,
		(CASE WHEN NOT a.is_balance_sheet AND x.fiscal_year = $3 THEN x.fiscal_period ELSE 0 END)::smallint AS period,
		sum(x.lc)::bigint, sum(x.gc)::bigint
	FROM x JOIN ledger.gl_account a ON a.id = x.account_id
	GROUP BY 1, 2, 3, 4
	HAVING sum(x.lc) <> 0 OR sum(x.gc) <> 0
	ORDER BY 1, 2 NULLS FIRST, 3, 4`

// BalancesThrough is the company's balances in the ledger through the
// fiscal year's period, per account and trading partner (BalanceThrough),
// those that are not zero in both currencies, by account, trading partner
// (none first), year and period.
func (ConsolidationReads) BalancesThrough(ctx context.Context, legalEntity, ledgerID uuid.UUID, year, period int16) ([]BalanceThrough, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, balancesThrough, legalEntity, ledgerID, year, period)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BalanceThrough, error) {
		var b BalanceThrough
		return b, r.Scan(&b.AccountID, &b.TradingPartnerID, &b.FiscalYear, &b.Period, &b.LC, &b.GC)
	})
	return out, errs.Wrapf(err, "ledger: balances of %s through %d/%d", legalEntity, year, period)
}

// RateOn is the latest rate of the type from one currency to another
// valid on the date, in either direction (rateOfType), with the date it is
// valid from; found is false when there is none. Rate is the 12-decimal
// form the ledger records (Shown), so a consumer translating at it can be
// reproduced from the rate it records.
func (ConsolidationReads) RateOn(ctx context.Context, rateType, from, to string, date time.Time) (RateValid, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return RateValid{}, false, err
	}
	x, found, err := rateOfType(ctx, tx, rateType, from, to, date)
	if err != nil || !found {
		return RateValid{}, false, err
	}
	return RateValid{Rate: money.ParseRat(x.shown), Shown: x.shown, ValidFrom: x.validFrom}, true, nil
}
