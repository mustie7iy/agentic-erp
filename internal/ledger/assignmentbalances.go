package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// AssignmentBalances is what the ledger publishes to the controlling
// context for plan values and plan against actual (ADR-0050 §7, §8, T56g,
// T56g3) and for internal orders (ADR-0064 §1, §2: the cost center an order
// names and the order's actuals): what a plan may name, the cost or profit centers of a hierarchy
// node, and the actuals by cost or profit center and account, read as the
// center reports read them (costCenterBalances, profitCenterBalances). It
// runs in the request's transaction under row-level security (ADR-0003);
// controlling reads no ledger table itself.
type AssignmentBalances struct{}

// PlanObjects are the objects among those asked for that a plan of a legal
// entity's fiscal year may name: its active cost centers, the active
// profit centers assigned to it, the profit and loss accounts set up in it
// (a plan compares with the center reports, which have only those), and
// the periods of the year in its calendar. PlanLocked are, per cost
// center asked for, the periods of the year in which it is locked for
// planning (T56g2): a period sharing a day with one of the center's
// periods whose lock_plan is set, in the calendar of any of the company's
// ledgers.
type PlanObjects struct {
	CostCenters, ProfitCenters, Accounts map[uuid.UUID]bool
	Periods                              map[int16]bool
	PlanLocked                           map[uuid.UUID]map[int16]bool
}

// PlanObjects reads which of the cost centers, profit centers and accounts
// a plan of the legal entity's fiscal year may name, the year's periods
// (none when the year is not set up) and the cost centers' plan locks. It
// holds the cost centers FOR KEY SHARE until the transaction ends, as a
// posting does (assign), and reads their periods after that, in a later
// statement: every change of a cost center's periods first holds the
// center FOR UPDATE (decided, applyCostCenterChange), so a plan meeting a
// lock under way waits for it and then reads the periods it wrote, and a
// lock meeting a plan under way waits for the plan (ADR-0008, ADR-0050 §8).
func (AssignmentBalances) PlanObjects(ctx context.Context, le uuid.UUID, year int16, costCenters, profitCenters, accounts []uuid.UUID) (PlanObjects, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PlanObjects{}, err
	}
	var out PlanObjects
	for _, q := range []struct {
		what string
		into *map[uuid.UUID]bool
		sql  string
		ids  []uuid.UUID
	}{
		{"cost centers", &out.CostCenters, `SELECT id FROM ledger.cost_center WHERE legal_entity_id = $1 AND status = 'active' AND id = ANY($2)
			ORDER BY id FOR KEY SHARE`, costCenters},
		{"profit centers", &out.ProfitCenters, `SELECT p.id FROM ledger.profit_center p JOIN ledger.profit_center_company c
			ON c.tenant_id = p.tenant_id AND c.profit_center_id = p.id AND c.legal_entity_id = $1 WHERE p.status = 'active' AND p.id = ANY($2)`, profitCenters},
		{"accounts", &out.Accounts, `SELECT a.id FROM ledger.gl_account a JOIN ledger.company_account ca
			ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1 WHERE NOT a.is_balance_sheet AND a.id = ANY($2)`, accounts},
	} {
		rows, _ := tx.Query(ctx, q.sql, le, q.ids)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return PlanObjects{}, fmt.Errorf("ledger: plan %s of %s: %w", q.what, le, err)
		}
		*q.into = make(map[uuid.UUID]bool, len(ids))
		for _, id := range ids {
			(*q.into)[id] = true
		}
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT period FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year = $2`, le, year)
	periods, err := pgx.CollectRows(rows, pgx.RowTo[int16])
	if err != nil {
		return PlanObjects{}, fmt.Errorf("ledger: periods of %s in %d: %w", le, year, err)
	}
	out.Periods = make(map[int16]bool, len(periods))
	for _, p := range periods {
		out.Periods[p] = true
	}
	out.PlanLocked, err = planLocks(ctx, tx, le, year, costCenters)
	return out, err
}

// planLocks are the periods of the legal entity's fiscal year in which
// each of the cost centers is locked for planning: those sharing a day
// with an active period of the center whose lock_plan is set (valid_to
// exclusive, a fiscal period's end_date inclusive), in the calendar of any
// of the company's ledgers, so a plan is never let in on a locked day.
func planLocks(ctx context.Context, tx pgx.Tx, le uuid.UUID, year int16, costCenters []uuid.UUID) (map[uuid.UUID]map[int16]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT p.cost_center_id, s.period FROM ledger.cost_center_period p
		JOIN ledger.period_status s ON s.tenant_id = p.tenant_id AND s.legal_entity_id = p.legal_entity_id AND s.fiscal_year = $2
		JOIN ledger.fiscal_period f ON f.tenant_id = s.tenant_id AND f.calendar_id = s.calendar_id AND f.fiscal_year = s.fiscal_year AND f.period = s.period
		WHERE p.legal_entity_id = $1 AND p.cost_center_id = ANY($3) AND p.retired_by_change_id IS NULL AND p.lock_plan
			AND daterange(p.valid_from, p.valid_to) && daterange(f.start_date, f.end_date, '[]')`, le, year, costCenters)
	out := map[uuid.UUID]map[int16]bool{}
	var center uuid.UUID
	var period int16
	_, err := pgx.ForEachRow(rows, []any{&center, &period}, func() error {
		if out[center] == nil {
			out[center] = map[int16]bool{}
		}
		out[center][period] = true
		return nil
	})
	return out, errs.Wrapf(err, "ledger: plan locks of the cost centers of %s in %d", le, year)
}

// CostCenterRef is a cost center as the controlling context names it on an
// internal order (ADR-0064 §1): its company, its code and whether it is
// active (approved, not pending or rejected).
type CostCenterRef struct {
	LegalEntityID uuid.UUID
	Code          string
	Active        bool
}

// CostCenterRefs reads the tenant's cost centers with the ids, keyed by
// id; an id that is no cost center has no entry. An active cost center
// stays one (a center change never takes a status back), so the read
// takes no lock.
func (AssignmentBalances) CostCenterRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CostCenterRef, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, legal_entity_id, code, status = $2 FROM ledger.cost_center WHERE id = ANY($1)`, ids, CenterActive)
	out := map[uuid.UUID]CostCenterRef{}
	var id uuid.UUID
	var c CostCenterRef
	_, err = pgx.ForEachRow(rows, []any{&id, &c.LegalEntityID, &c.Code, &c.Active}, func() error { out[id] = c; return nil })
	return out, errs.Wrapf(err, "ledger: cost centers named by the controlling context")
}

// CostCenters are the cost centers a plan against actual is limited to,
// as the cost center report limits its rows (reportCenters): nil for all,
// the one asked for, or those at the node of a cost center hierarchy and
// below it under today's tree; with both, the one if it is among them. ok
// is false when none remain.
func (AssignmentBalances) CostCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error) {
	return assignmentCenters(ctx, costCenterBalances, center, node)
}

// ProfitCenters are the profit centers a plan against actual is limited
// to, as the profit center report limits its rows: as CostCenters, with a
// node of a profit center hierarchy (T56g3).
func (AssignmentBalances) ProfitCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error) {
	return assignmentCenters(ctx, profitCenterBalances, center, node)
}

func assignmentCenters(ctx context.Context, k reportKind, center, node *uuid.UUID) ([]uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, false, err
	}
	return reportCenters(ctx, tx, k, center, node)
}

// CenterAccount is the key of a row of actuals: a center (cost or profit,
// as asked for) and an account. Rows come in its order, by center, then
// account.
type CenterAccount struct{ CenterID, AccountID uuid.UUID }

// AssignmentActual is the actual of a center on an account over the
// periods asked for, in local currency, debit positive.
type AssignmentActual struct {
	CenterAccount
	Amount money.Minor
}

// ActualsQuery asks for the actuals of a legal entity's centers in a
// range of periods of a fiscal year: of the centers given (nil: every
// one), with keys after After (nil: from the first), at most Limit rows.
type ActualsQuery struct {
	LegalEntityID        uuid.UUID
	FiscalYear           int16
	PeriodFrom, PeriodTo int16
	Centers              []uuid.UUID
	After                *CenterAccount
	Limit                int
}

// CostCenterActuals reads the leading ledger's actuals by cost center and
// account (plan compares with the leading ledger, ADR-0050 §8) as the cost
// center report reads them: the projected balances plus the lines of
// entries still queued, so they are exact at the read. A tenant without a
// leading ledger has none.
func (AssignmentBalances) CostCenterActuals(ctx context.Context, q ActualsQuery) ([]AssignmentActual, error) {
	return actuals(ctx, costCenterBalances, q)
}

// ProfitCenterActuals reads the leading ledger's actuals by profit center
// and account as the profit center report reads them (T56g3): the profit
// center each line carries, entered or derived at posting (§5), summed
// over the segments, on profit and loss accounts only, exact at the read.
func (AssignmentBalances) ProfitCenterActuals(ctx context.Context, q ActualsQuery) ([]AssignmentActual, error) {
	return actuals(ctx, profitCenterBalances, q)
}

// actuals is planned per call (pagination.PlanPerCall), as a listing is:
// its centers, its period range and its page's start are optional, and
// from a cached statement's sixth run PostgreSQL took a generic plan
// that keeps them as filters (360,000 balances: ten profit centers'
// actuals 0.8 ms to 2.6-8.8 ms, every center's 162 ms to 178-201 ms).
func actuals(ctx context.Context, k reportKind, q ActualsQuery) ([]AssignmentActual, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	leading, ok, err := ledgerOrLeading(ctx, tx, nil)
	if err != nil || !ok {
		return nil, err
	}
	var afterCenter, afterAccount *uuid.UUID
	if q.After != nil {
		afterCenter, afterAccount = &q.After.CenterID, &q.After.AccountID
	}
	pnl := ``
	if k.pnlOnly {
		pnl = ` AND ` + pnlAccount
	}
	// $1-$8 are the report's (rows); $9 and $10 the key the page starts after.
	rows, _ := tx.Query(ctx, `SELECT center_id, account_id, sum(debit_lc - credit_lc)::bigint FROM (`+k.rows()+`) b
		WHERE ($9::uuid IS NULL OR (center_id, account_id) > ($9, $10::uuid))`+pnl+`
		GROUP BY 1, 2 ORDER BY 1, 2 LIMIT $11`, pagination.PlanPerCall,
		q.LegalEntityID, leading, q.FiscalYear, q.PeriodFrom, q.PeriodTo, q.Centers, nil, nil, afterCenter, afterAccount, q.Limit)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AssignmentActual, error) {
		var a AssignmentActual
		return a, errs.Wrapf(r.Scan(&a.CenterID, &a.AccountID, &a.Amount), "scan "+k.table+" actual")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: %s actuals of %s in %d: %w", k.table, q.LegalEntityID, q.FiscalYear, err)
	}
	return out, nil
}

// OrderActualKey is the key of a row of an internal order's actuals: an
// account, by its number, and a fiscal period. Rows come in its order.
type OrderActualKey struct {
	AccountNumber string
	AccountID     uuid.UUID
	Period        int16
}

// OrderActual is what was posted to an internal order on an account in a
// period, in local currency: the debits, the credits (positive) and the
// debits less the credits.
type OrderActual struct {
	OrderActualKey
	AccountName            string
	Debit, Credit, Balance money.Minor
}

// OrderActualsQuery asks for the actuals of an internal order of a legal
// entity in a range of periods of a fiscal year: the rows with keys after
// After (nil: from the first), at most Limit.
type OrderActualsQuery struct {
	LegalEntityID, OrderID uuid.UUID
	FiscalYear             int16
	PeriodFrom, PeriodTo   int16
	After                  *OrderActualKey
	Limit                  int
}

// OrderActuals are a page of an order's actuals and the totals of the
// whole selection, whatever the page.
type OrderActuals struct {
	Rows                   []OrderActual
	Debit, Credit, Balance money.Minor
}

// orderLines are the leading ledger's lines of an internal order in the
// periods asked for ($1-$6), found by the order's partial index (000270).
const orderLines = ` FROM ledger.journal_line l WHERE l.legal_entity_id = $1 AND l.ledger_id = $2 AND l.internal_order_id = $3
	AND l.fiscal_year = $4 AND l.fiscal_period BETWEEN $5 AND $6`

// orderSums are a selection of lines' debits, credits and balance.
const orderSums = `coalesce(sum(l.amount_lc) FILTER (WHERE l.amount_lc > 0), 0)::bigint, coalesce(-sum(l.amount_lc) FILTER (WHERE l.amount_lc < 0), 0)::bigint,
	coalesce(sum(l.amount_lc), 0)::bigint`

// OrderActuals reads the leading ledger's actuals of an internal order by
// account and period (ADR-0064 §2, T60b): the journal lines that name the
// order, a reversal's included, read from the lines themselves, so they
// are exact at the read. A tenant without a leading ledger has none.
func (AssignmentBalances) OrderActuals(ctx context.Context, q OrderActualsQuery) (OrderActuals, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return OrderActuals{}, err
	}
	leading, ok, err := ledgerOrLeading(ctx, tx, nil)
	if err != nil || !ok {
		return OrderActuals{}, err
	}
	var out OrderActuals
	if err := tx.QueryRow(ctx, `SELECT `+orderSums+orderLines, q.LegalEntityID, leading, q.OrderID, q.FiscalYear, q.PeriodFrom, q.PeriodTo).
		Scan(&out.Debit, &out.Credit, &out.Balance); err != nil {
		return OrderActuals{}, fmt.Errorf("ledger: total actuals of internal order %s in %d: %w", q.OrderID, q.FiscalYear, err)
	}
	var number *string
	var account *uuid.UUID
	var period *int16
	if q.After != nil {
		number, account, period = &q.After.AccountNumber, &q.After.AccountID, &q.After.Period
	}
	rows, _ := tx.Query(ctx, `SELECT a.number, s.account_id, s.fiscal_period, a.name, s.debit, s.credit, s.balance
		FROM (SELECT l.tenant_id, l.account_id, l.fiscal_period, `+orderSums+orderLines+` GROUP BY 1, 2, 3) AS s(tenant_id, account_id, fiscal_period, debit, credit, balance)
		JOIN ledger.gl_account a ON a.tenant_id = s.tenant_id AND a.id = s.account_id
		WHERE $7::text IS NULL OR (a.number, s.account_id, s.fiscal_period) > ($7, $8::uuid, $9::smallint)
		ORDER BY a.number, s.account_id, s.fiscal_period LIMIT $10`,
		q.LegalEntityID, leading, q.OrderID, q.FiscalYear, q.PeriodFrom, q.PeriodTo, number, account, period, q.Limit)
	out.Rows, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrderActual, error) {
		var a OrderActual
		return a, errs.Wrapf(r.Scan(&a.AccountNumber, &a.AccountID, &a.Period, &a.AccountName, &a.Debit, &a.Credit, &a.Balance), "scan internal order actual")
	})
	if err != nil {
		return OrderActuals{}, fmt.Errorf("ledger: actuals of internal order %s in %d: %w", q.OrderID, q.FiscalYear, err)
	}
	return out, nil
}
