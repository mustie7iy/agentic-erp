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
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Actual reports by account assignment (ADR-0050 §7, T56f): the period
// totals of a company's cost centers and profit centers, read as the trial
// balance is (balanceRows): the balances the projector keeps by assignment
// (ledger.assignment_balance, migration 000163) plus the lines of entries
// it has not reached yet, so the figures are exact at the moment of the
// read. Only profit and loss accounts are reported: a balance sheet line
// carries no cost center, and its profit center, entered only, gives no
// balance sheet by profit center without document splitting (decision P3).

// reportKind is what tells the two reports apart: the line's column the
// rows are by, the centers' table, the hierarchy kind a node filter takes,
// whether the rows show the lines' segment (the profit center's; a cost
// center's rows sum its lines of every segment), and whether balance sheet
// lines are left out: they carry no cost center (the posting refuses one on
// an account whose cost element is none, as every balance sheet account's
// is, ADR-0050 §5), but may carry a profit center entered.
type reportKind struct {
	column, table, hierarchy string
	bySegment, pnlOnly       bool
}

var (
	costCenterBalances   = reportKind{column: "cost_center_id", table: "cost_center", hierarchy: HierarchyCostCenters}
	profitCenterBalances = reportKind{column: "profit_center_id", table: "profit_center", hierarchy: HierarchyProfitCenters, bySegment: true, pnlOnly: true}
)

// rows sums, per center, segment, account, period and transaction
// currency, the projected balances and the pending lines of $1 legal
// entity, $2 ledger and $3 fiscal year in periods $4 to $5, of $6 centers
// (NULL: all), $7 segment and $8 account (both optional).
func (k reportKind) rows() string {
	filter := func(t string) string {
		center := pgx.Identifier{t, k.column}.Sanitize()
		return ` AND ` + center + ` IS NOT NULL AND ` + t + `.fiscal_period BETWEEN $4 AND $5 AND ($6::uuid[] IS NULL OR ` + center +
			` = ANY($6)) AND ($7::uuid IS NULL OR ` + t + `.segment_id = $7) AND ($8::uuid IS NULL OR ` + t + `.account_id = $8)`
	}
	segment := func(t string) string {
		if k.bySegment {
			return t + `.segment_id`
		}
		return `NULL::uuid`
	}
	return `SELECT center_id, segment_id, account_id, fiscal_period, currency_tc, sum(debit_tc)::bigint AS debit_tc, sum(credit_tc)::bigint AS credit_tc,
			sum(debit_lc)::bigint AS debit_lc, sum(credit_lc)::bigint AS credit_lc, sum(debit_gc)::bigint AS debit_gc, sum(credit_gc)::bigint AS credit_gc,
			max(updated_at) AS updated_at
		FROM (
			SELECT ` + pgx.Identifier{"b", k.column}.Sanitize() + ` AS center_id, ` + segment("b") + ` AS segment_id, b.account_id, b.fiscal_period, b.currency_tc, b.debit_tc, b.credit_tc,
				b.debit_lc, b.credit_lc, b.debit_gc, b.credit_gc, b.updated_at
			FROM ledger.assignment_balance b
			WHERE b.legal_entity_id = $1 AND b.ledger_id = $2 AND b.fiscal_year = $3` + filter("b") + `
			UNION ALL
			SELECT ` + pgx.Identifier{"l", k.column}.Sanitize() + `, ` + segment("l") + `, l.account_id, l.fiscal_period, e.currency_tc, greatest(l.amount_tc, 0), greatest(-l.amount_tc, 0),
				greatest(l.amount_lc, 0), greatest(-l.amount_lc, 0), greatest(l.amount_gc, 0), greatest(-l.amount_gc, 0), e.posted_at
			FROM ledger.balance_projection q
			JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
			JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
			WHERE q.projected_at IS NULL AND q.fiscal_year = $3 AND e.legal_entity_id = $1 AND e.ledger_id = $2` + filter("l") + `
		) u
		GROUP BY 1, 2, 3, 4, 5`
}

// selectRows is rows with each row's id (from center, segment, account,
// period and currency, for paging), the center's and segment's codes and
// the currency's exponent, on profit and loss accounts only. The codes
// and the exponent are scalar subqueries, which PostgreSQL evaluates for
// the page's rows only, after the sort and the limit, not for the period's
// every row.
func (k reportKind) selectRows() string {
	pnl := ``
	if k.pnlOnly {
		pnl = ` WHERE EXISTS (SELECT FROM ledger.gl_account a WHERE a.id = b.account_id AND NOT a.is_balance_sheet)`
	}
	return `SELECT * FROM (SELECT md5(b.center_id::text || '/' || coalesce(b.segment_id::text, '-') || '/' || b.account_id || '/' || b.fiscal_period
			|| '/' || b.currency_tc)::uuid AS id, b.center_id,
			(SELECT c.code FROM ` + pgx.Identifier{"ledger", k.table}.Sanitize() + ` c WHERE c.id = b.center_id) AS center_code,
			b.segment_id, (SELECT s.code FROM ledger.segment s WHERE s.id = b.segment_id) AS segment_code, b.account_id, b.fiscal_period, b.currency_tc,
			(SELECT cur.exponent FROM ref.currency cur WHERE cur.code = b.currency_tc) AS exponent,
			b.debit_tc, b.credit_tc, b.debit_lc, b.credit_lc, b.debit_gc, b.credit_gc, b.updated_at
		FROM (` + k.rows() + `) b` + pnl + `) t`
}

// CenterBalance is the period totals of one center (and, by profit
// center, its segment) on one account in one transaction currency.
type CenterBalance struct {
	ID                uuid.UUID // derived from its key, for paging
	CenterID          uuid.UUID
	CenterCode        string
	SegmentID         *uuid.UUID
	SegmentCode       *string
	AccountID         uuid.UUID
	FiscalPeriod      int16
	CurrencyTC        string
	ExpTC             int
	DebitTC, CreditTC money.Minor
	DebitLC, CreditLC money.Minor
	DebitGC, CreditGC money.Minor
	UpdatedAt         time.Time
}

func scanCenterBalance(row pgx.CollectableRow) (CenterBalance, error) {
	var b CenterBalance
	err := row.Scan(&b.ID, &b.CenterID, &b.CenterCode, &b.SegmentID, &b.SegmentCode, &b.AccountID, &b.FiscalPeriod, &b.CurrencyTC, &b.ExpTC,
		&b.DebitTC, &b.CreditTC, &b.DebitLC, &b.CreditLC, &b.DebitGC, &b.CreditGC, &b.UpdatedAt)
	return b, errs.Wrapf(err, "scan center balance")
}

// centerQuery is a report's filters, as both operations take them.
type centerQuery struct {
	LegalEntityID        uuid.UUID
	LedgerID             *uuid.UUID
	FiscalYear           int32
	PeriodFrom, PeriodTo gen.OptInt32
	CenterID, NodeID     *uuid.UUID
	SegmentID, AccountID *uuid.UUID
	OrderBy, PageToken   gen.OptString
	PageSize             gen.OptInt32
}

// centerReport is a page of a report with what its rows are shown in.
type centerReport struct {
	rows   []CenterBalance
	next   string
	ledger uuid.UUID
	le     tenancy.Entity
	group  string
	exps   currency.Exponents
}

func centerSort(column string) pagination.Sort {
	return pagination.Sort{Default: column, Fields: []pagination.Field{
		{Name: column, Column: "center_id", Type: "uuid"}, {Name: "account_id", Column: "account_id", Type: "uuid"},
		{Name: "fiscal_period", Column: "fiscal_period", Type: "smallint"},
	}}
}

// periods are the report's periods, all of the year by default; a range
// that ends before it starts is refused.
func (q centerQuery) periods() (from, to int32, err error) {
	from, to = q.PeriodFrom.Or(1), q.PeriodTo.Or(16)
	if from > to {
		return 0, 0, problem.BadRequest("request.invalid_parameter", "fiscal_period_to is before fiscal_period_from.",
			problem.FieldError{Field: "fiscal_period_to", Code: "before_start", Message: "on or after fiscal_period_from"})
	}
	return from, to, nil
}

// report reads a page of a center report. It has no rows when the legal
// entity is one the caller cannot act in, the ledger does not exist or no
// center of the kind is asked for (a node without centers).
func (e *Endpoints) report(ctx context.Context, k reportKind, q centerQuery) (centerReport, error) {
	var r, none centerReport
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return none, err
	}
	from, to, err := q.periods()
	if err != nil {
		return none, err
	}
	scope := fmt.Sprintf("%s|%s|%v|%d|%d|%d|%v|%v|%v|%v", k.table, q.LegalEntityID, q.LedgerID, q.FiscalYear, from, to, q.CenterID, q.NodeID, q.SegmentID, q.AccountID)
	ks, err := e.Pages.Keyset(centerSort(k.column), scope, apitypes.Page(q.OrderBy, q.PageSize, q.PageToken))
	if err != nil {
		return none, err
	}
	var ok bool
	if r.le, ok, err = e.reportEntity(ctx, q.LegalEntityID); err != nil || !ok {
		return none, err
	}
	if r.ledger, ok, err = ledgerOrLeading(ctx, tx, q.LedgerID); err != nil || !ok {
		return none, err
	}
	centers, ok, err := reportCenters(ctx, tx, k, q.CenterID, q.NodeID)
	if err != nil || !ok {
		return none, err
	}
	if r.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return none, err
	}
	if r.exps, err = exponents(ctx, tx, r.le.LocalCurrency, r.group); err != nil {
		return none, err
	}
	sql, args := ks.SQL(k.selectRows()+` WHERE true`, []any{r.le.ID, r.ledger, q.FiscalYear, from, to, centers, q.SegmentID, q.AccountID})
	rows, _ := tx.Query(ctx, sql, args...)
	balances, err := pgx.CollectRows(rows, scanCenterBalance)
	if err != nil {
		return centerReport{}, fmt.Errorf("ledger: %s balances of %s: %w", k.table, r.le.ID, err)
	}
	r.rows, r.next = pagination.Page(ks, balances, func(b CenterBalance) (string, uuid.UUID) {
		switch ks.Field() {
		case "account_id":
			return b.AccountID.String(), b.ID
		case "fiscal_period":
			return fmt.Sprint(b.FiscalPeriod), b.ID
		}
		return b.CenterID.String(), b.ID
	})
	return r, nil
}

// reportEntity is the legal entity of a report, when the caller may act in
// it.
func (e *Endpoints) reportEntity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, false, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	if err != nil || !ok || !access.Allows(le.ID) {
		return tenancy.Entity{}, false, err
	}
	return le, true, nil
}

// reportCenters are the centers a report is limited to: nil for all, the
// one asked for, or those at the node and below it under today's tree
// (decision P7), as GET /v1/hierarchies/{id}/nodes/{node_id} lists them;
// with both, the one if it is among them. ok is false when none remain.
func reportCenters(ctx context.Context, tx pgx.Tx, k reportKind, center, node *uuid.UUID) ([]uuid.UUID, bool, error) {
	if node == nil {
		if center == nil {
			return nil, true, nil
		}
		return []uuid.UUID{*center}, true, nil
	}
	var hierarchy uuid.UUID
	err := tx.QueryRow(ctx, `SELECT n.hierarchy_id FROM ledger.hierarchy_node n JOIN ledger.hierarchy h ON h.id = n.hierarchy_id
		WHERE n.id = $1 AND n.removed_by_change_id IS NULL AND h.kind = $2`, *node, k.hierarchy).Scan(&hierarchy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("ledger: hierarchy of node %s: %w", *node, err)
	}
	t, err := loadTree(ctx, tx, byID, false, hierarchy)
	if err != nil {
		return nil, false, err
	}
	var centers []uuid.UUID
	for _, n := range t.subtree(*node) {
		centers = append(centers, n.Centers...)
	}
	if center != nil {
		centers = slices.DeleteFunc(centers, func(c uuid.UUID) bool { return c != *center })
	}
	return centers, len(centers) > 0, nil
}

// amounts are a row's six totals as the API shows them.
func (r centerReport) amounts(b CenterBalance) (debit, credit, debitLC, creditLC, debitGC, creditGC gen.Money) {
	lc, gc := r.exps[r.le.LocalCurrency], r.exps[r.group]
	return apitypes.Money(b.DebitTC, b.CurrencyTC, b.ExpTC), apitypes.Money(b.CreditTC, b.CurrencyTC, b.ExpTC),
		apitypes.Money(b.DebitLC, r.le.LocalCurrency, lc), apitypes.Money(b.CreditLC, r.le.LocalCurrency, lc),
		apitypes.Money(b.DebitGC, r.group, gc), apitypes.Money(b.CreditGC, r.group, gc)
}

// ListCostCenterBalances implements GET /v1/cost-center-balances.
func (e *Endpoints) ListCostCenterBalances(ctx context.Context, params gen.ListCostCenterBalancesParams) (gen.ListCostCenterBalancesRes, error) {
	r, err := e.report(ctx, costCenterBalances, centerQuery{LegalEntityID: params.LegalEntityID, LedgerID: apitypes.Ptr[uuid.UUID](params.LedgerID),
		FiscalYear: params.FiscalYear, PeriodFrom: params.FiscalPeriodFrom, PeriodTo: params.FiscalPeriodTo,
		CenterID: apitypes.Ptr[uuid.UUID](params.CostCenterID), NodeID: apitypes.Ptr[uuid.UUID](params.HierarchyNodeID),
		SegmentID: apitypes.Ptr[uuid.UUID](params.SegmentID), AccountID: apitypes.Ptr[uuid.UUID](params.AccountID),
		OrderBy: params.OrderBy, PageToken: params.PageToken, PageSize: params.PageSize})
	if err != nil {
		return nil, err
	}
	page := gen.CostCenterBalancePage{Items: make([]gen.CostCenterBalance, len(r.rows))}
	for i, b := range r.rows {
		d, c, dl, cl, dg, cg := r.amounts(b)
		page.Items[i] = gen.CostCenterBalance{LedgerID: r.ledger, LegalEntityID: r.le.ID, CostCenterID: b.CenterID, CostCenterCode: b.CenterCode,
			AccountID: b.AccountID, FiscalYear: params.FiscalYear, FiscalPeriod: int32(b.FiscalPeriod), Currency: b.CurrencyTC,
			Debit: d, Credit: c, DebitLc: dl, CreditLc: cl, DebitGc: dg, CreditGc: cg, UpdatedAt: b.UpdatedAt}
	}
	if r.next != "" {
		page.NextPageToken.SetTo(r.next)
	}
	return &gen.CostCenterBalancePageHeaders{Response: page}, nil
}

// ListProfitCenterBalances implements GET /v1/profit-center-balances.
func (e *Endpoints) ListProfitCenterBalances(ctx context.Context, params gen.ListProfitCenterBalancesParams) (gen.ListProfitCenterBalancesRes, error) {
	r, err := e.report(ctx, profitCenterBalances, centerQuery{LegalEntityID: params.LegalEntityID, LedgerID: apitypes.Ptr[uuid.UUID](params.LedgerID),
		FiscalYear: params.FiscalYear, PeriodFrom: params.FiscalPeriodFrom, PeriodTo: params.FiscalPeriodTo,
		CenterID: apitypes.Ptr[uuid.UUID](params.ProfitCenterID), NodeID: apitypes.Ptr[uuid.UUID](params.HierarchyNodeID),
		SegmentID: apitypes.Ptr[uuid.UUID](params.SegmentID), AccountID: apitypes.Ptr[uuid.UUID](params.AccountID),
		OrderBy: params.OrderBy, PageToken: params.PageToken, PageSize: params.PageSize})
	if err != nil {
		return nil, err
	}
	page := gen.ProfitCenterBalancePage{Items: make([]gen.ProfitCenterBalance, len(r.rows))}
	for i, b := range r.rows {
		d, c, dl, cl, dg, cg := r.amounts(b)
		page.Items[i] = gen.ProfitCenterBalance{LedgerID: r.ledger, LegalEntityID: r.le.ID, ProfitCenterID: b.CenterID, ProfitCenterCode: b.CenterCode,
			SegmentID: apitypes.OptUUID(b.SegmentID), SegmentCode: apitypes.OptString(b.SegmentCode), AccountID: b.AccountID,
			FiscalYear: params.FiscalYear, FiscalPeriod: int32(b.FiscalPeriod), Currency: b.CurrencyTC,
			Debit: d, Credit: c, DebitLc: dl, CreditLc: cl, DebitGc: dg, CreditGc: cg, UpdatedAt: b.UpdatedAt}
	}
	if r.next != "" {
		page.NextPageToken.SetTo(r.next)
	}
	return &gen.ProfitCenterBalancePageHeaders{Response: page}, nil
}
