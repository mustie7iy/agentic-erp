package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// AssignmentBalances is what the ledger publishes to the controlling
// context for plan values and plan against actual (ADR-0050 §7, §8, T56g):
// what a plan may name, the cost centers of a hierarchy node, and the
// actuals by cost center and account, read as the center reports read them
// (costCenterBalances). It runs in the request's transaction under
// row-level security (ADR-0003); controlling reads no ledger table itself.
type AssignmentBalances struct{}

// PlanObjects are the objects among those asked for that a plan of a legal
// entity's fiscal year may name: its active cost centers, the active
// profit centers assigned to it, the profit and loss accounts set up in it
// (a plan compares with the center reports, which have only those), and
// the periods of the year in its calendar.
type PlanObjects struct {
	CostCenters, ProfitCenters, Accounts map[uuid.UUID]bool
	Periods                              map[int16]bool
}

// PlanObjects reads which of the cost centers, profit centers and accounts
// a plan of the legal entity's fiscal year may name, and the year's
// periods (none when the year is not set up).
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
		{"cost centers", &out.CostCenters, `SELECT id FROM ledger.cost_center WHERE legal_entity_id = $1 AND status = 'active' AND id = ANY($2)`, costCenters},
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
	return out, nil
}

// CostCenters are the cost centers a plan against actual is limited to,
// as the cost center report limits its rows (reportCenters): nil for all,
// the one asked for, or those at the node of a cost center hierarchy and
// below it under today's tree; with both, the one if it is among them. ok
// is false when none remain.
func (AssignmentBalances) CostCenters(ctx context.Context, center, node *uuid.UUID) ([]uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, false, err
	}
	return reportCenters(ctx, tx, costCenterBalances, center, node)
}

// CenterAccount is the key of a row of actuals: a cost center and an
// account. Rows come in its order, by cost center, then account.
type CenterAccount struct{ CostCenterID, AccountID uuid.UUID }

// AssignmentActual is the actual of a cost center on an account over the
// periods asked for, in local currency, debit positive.
type AssignmentActual struct {
	CenterAccount
	Amount money.Minor
}

// ActualsQuery asks for the actuals of a legal entity's cost centers in a
// range of periods of a fiscal year: of the centers given (nil: every
// one), with keys after After (nil: from the first), at most Limit rows.
type ActualsQuery struct {
	LegalEntityID        uuid.UUID
	FiscalYear           int16
	PeriodFrom, PeriodTo int16
	CostCenters          []uuid.UUID
	After                *CenterAccount
	Limit                int
}

// CostCenterActuals reads the leading ledger's actuals by cost center and
// account (plan compares with the leading ledger, ADR-0050 §8) as the cost
// center report reads them: the projected balances plus the lines of
// entries still queued, so they are exact at the read. A tenant without a
// leading ledger has none.
func (AssignmentBalances) CostCenterActuals(ctx context.Context, q ActualsQuery) ([]AssignmentActual, error) {
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
		afterCenter, afterAccount = &q.After.CostCenterID, &q.After.AccountID
	}
	// $1-$8 are the report's (rows); $9 and $10 the key the page starts after.
	rows, _ := tx.Query(ctx, `SELECT center_id, account_id, sum(debit_lc - credit_lc)::bigint FROM (`+costCenterBalances.rows()+`) b
		WHERE $9::uuid IS NULL OR (center_id, account_id) > ($9, $10::uuid)
		GROUP BY 1, 2 ORDER BY 1, 2 LIMIT $11`,
		q.LegalEntityID, leading, q.FiscalYear, q.PeriodFrom, q.PeriodTo, q.CostCenters, nil, nil, afterCenter, afterAccount, q.Limit)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AssignmentActual, error) {
		var a AssignmentActual
		return a, errs.Wrapf(r.Scan(&a.CostCenterID, &a.AccountID, &a.Amount), "scan cost center actual")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: cost center actuals of %s in %d: %w", q.LegalEntityID, q.FiscalYear, err)
	}
	return out, nil
}
