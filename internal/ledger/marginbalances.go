package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The margin report's figures (ADR-0075 §3, T92b): a company's revenue
// and cost of sales by profit center, read from the balances the two
// center-neutral and center-wise reports read (period_balance and
// assignment_balance, each with the lines of entries the projector has
// not reached yet), in one statement, so the company's totals, the
// profit centers' rows and what no profit center carries are of one
// database state and add up.

// MarginAmounts are a revenue (credits less debits on revenue accounts),
// a cost of sales (debits less credits on the accounts asked for) and
// their difference, in local currency.
type MarginAmounts struct{ Revenue, CostOfSales, Margin money.Minor }

// MarginKey is the key of a row of the margin report: a profit center by
// its code. Rows come in its order.
type MarginKey struct {
	Code string
	ID   uuid.UUID
}

// MarginRow is a profit center's revenue, cost of sales and margin.
type MarginRow struct {
	MarginKey
	Name string
	MarginAmounts
}

// MarginQuery asks for the margin of a legal entity in a range of periods
// of a fiscal year, with the cost of sales on the accounts with the
// numbers given: the profit centers' rows after After (nil: from the
// first), at most Limit.
type MarginQuery struct {
	LegalEntityID        uuid.UUID
	FiscalYear           int16
	PeriodFrom, PeriodTo int16
	CostOfSales          []string
	After                *MarginKey
	Limit                int
}

// Margin is a page of the profit centers' rows with the company's totals
// and what the lines without a profit center add to them (Company less
// every profit center's row), whatever the page.
type Margin struct {
	Rows                []MarginRow
	Company, Unassigned MarginAmounts
}

// marginAccounts are the company's accounts the margin is read on ($1 the
// legal entity, $9 the cost of sales accounts' numbers): its revenue
// accounts, and the expense accounts among the numbers.
const marginAccounts = `SELECT a.id, a.account_type = 'revenue' AS revenue FROM ledger.gl_account a
	JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
	WHERE a.account_type = 'revenue' OR (a.account_type = 'expense' AND a.number = ANY($9))`

// marginCompany are the company's balances by account, credit positive,
// in periods $4 to $5 of $3 fiscal year in $2 ledger, as the trial
// balance reads them (balanceRows): projected, and still queued.
const marginCompany = `SELECT b.account_id, b.credit_lc - b.debit_lc AS net FROM ledger.period_balance b
	WHERE b.legal_entity_id = $1 AND b.ledger_id = $2 AND b.fiscal_year = $3 AND b.fiscal_period BETWEEN $4 AND $5
	UNION ALL
	SELECT l.account_id, -l.amount_lc FROM ledger.balance_projection q
	JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
	JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
	WHERE q.projected_at IS NULL AND q.fiscal_year = $3 AND e.legal_entity_id = $1 AND e.ledger_id = $2 AND l.fiscal_period BETWEEN $4 AND $5`

// marginSums are a selection's revenue and cost of sales from rows with a
// net amount, credit positive, joined to acc. The sums are integers, so a
// margin is exact, and one beyond a bigint fails the statement rather
// than wrap.
const marginSums = `coalesce(sum(u.net) FILTER (WHERE acc.revenue), 0)::bigint AS revenue, coalesce(-sum(u.net) FILTER (WHERE NOT acc.revenue), 0)::bigint AS cost`

// Margin reads the leading ledger's margin figures (ADR-0075 §3): the
// profit center each line carries is the one the profit center report
// shows it on (profitCenterBalances), a profit center with neither
// revenue nor cost of sales in the periods has no row, and a tenant
// without a leading ledger has none. It is planned per call
// (pagination.PlanPerCall), as actuals is: its page's start is optional.
func (AssignmentBalances) Margin(ctx context.Context, q MarginQuery) (Margin, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Margin{}, err
	}
	leading, ok, err := ledgerOrLeading(ctx, tx, nil)
	if err != nil || !ok {
		return Margin{}, err
	}
	var afterCode *string
	var afterID *uuid.UUID
	if q.After != nil {
		afterCode, afterID = &q.After.Code, &q.After.ID
	}
	// $1-$8 are the profit center report's (rows); $9 the cost of sales
	// accounts, $10 and $11 the key the page starts after, $12 its size.
	rows, _ := tx.Query(ctx, `WITH acc AS MATERIALIZED (`+marginAccounts+`),
		company AS (SELECT `+marginSums+` FROM (`+marginCompany+`) u JOIN acc ON acc.id = u.account_id),
		centers AS (SELECT u.center_id, `+marginSums+` FROM (SELECT b.center_id, b.account_id, b.credit_lc - b.debit_lc AS net
			FROM (`+profitCenterBalances.rows()+`) b) u JOIN acc ON acc.id = u.account_id GROUP BY 1),
		assigned AS (SELECT coalesce(sum(revenue), 0)::bigint AS revenue, coalesce(sum(cost), 0)::bigint AS cost FROM centers)
		SELECT 0 AS kind, NULL::uuid AS id, '' AS code, '' AS name, c.revenue, c.cost, c.revenue - c.cost FROM company c
		UNION ALL
		SELECT 1, NULL, '', '', c.revenue - a.revenue, c.cost - a.cost, (c.revenue - a.revenue) - (c.cost - a.cost) FROM company c, assigned a
		UNION ALL
		(SELECT 2, p.id, p.code, p.name, c.revenue, c.cost, c.revenue - c.cost FROM centers c JOIN ledger.profit_center p ON p.id = c.center_id
			WHERE (c.revenue <> 0 OR c.cost <> 0) AND ($10::text IS NULL OR (p.code, p.id) > ($10, $11::uuid)) ORDER BY p.code, p.id LIMIT $12)
		ORDER BY kind, code, id`, pagination.PlanPerCall,
		q.LegalEntityID, leading, q.FiscalYear, q.PeriodFrom, q.PeriodTo, nil, nil, nil, q.CostOfSales, afterCode, afterID, q.Limit)
	out := Margin{Rows: []MarginRow{}}
	var kind int
	var id *uuid.UUID
	var r MarginRow
	_, err = pgx.ForEachRow(rows, []any{&kind, &id, &r.Code, &r.Name, &r.Revenue, &r.CostOfSales, &r.Margin}, func() error {
		switch kind {
		case 0:
			out.Company = r.MarginAmounts
		case 1:
			out.Unassigned = r.MarginAmounts
		default:
			r.ID = *id
			out.Rows = append(out.Rows, r)
		}
		return nil
	})
	return out, errs.Wrapf(err, "ledger: margin of %s in %d", q.LegalEntityID, q.FiscalYear)
}
