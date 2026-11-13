package controlling

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The margin report (ADR-0075, T92b): a company's revenue, cost of sales
// and margin by profit center over a range of periods, from the leading
// ledger's balances. Revenue is the accounts of type revenue; cost of
// sales the accounts the configuration names for the company's chart
// (controlling.margin_report), which a person posts to, since nothing
// posts a cost of goods sold. The ledger reads the figures in one
// statement (ledger.AssignmentBalances.Margin), so the rows, the
// unassigned row and the totals add up at the moment of the read.

// mrCursor is where a page of the margin report starts: after the profit
// center with the code and id, or, with Tail, after the last of them,
// where only the unassigned row is left.
type mrCursor struct {
	Code string    `json:"c,omitempty"`
	ID   uuid.UUID `json:"i,omitzero"`
	Tail bool      `json:"t,omitempty"`
}

// mrQuery is a page's request, resolved.
type mrQuery struct {
	le      tenancy.Entity
	exp     int
	numbers []string // the cost of sales accounts
	named   bool     // whether the configuration names any for the chart
	margin  ledger.MarginQuery
	scope   string
	size    int
	tail    bool
}

// GetMarginReport implements GET /v1/margin-report.
func (e *Endpoints) GetMarginReport(ctx context.Context, params gen.GetMarginReportParams) (gen.GetMarginReportRes, error) {
	q, err := e.marginQuery(ctx, params)
	if err != nil {
		return nil, err
	}
	m, err := e.Ledger.Margin(ctx, q.margin)
	if err != nil {
		return nil, fmt.Errorf("controlling: margin of %s: %w", q.le.ID, err)
	}
	amount := func(v money.Minor) gen.Money { return apitypes.Money(v, q.le.LocalCurrency, q.exp) }
	row := func(a ledger.MarginAmounts) gen.MarginReportRow {
		return gen.MarginReportRow{Revenue: amount(a.Revenue), CostOfSales: amount(a.CostOfSales), Margin: amount(a.Margin)}
	}
	out := &gen.MarginReport{LegalEntityID: q.le.ID, FiscalYear: params.FiscalYear, FiscalPeriodFrom: int32(q.margin.PeriodFrom),
		FiscalPeriodTo: int32(q.margin.PeriodTo), Currency: q.le.LocalCurrency, CostOfSalesNamed: q.named, CostOfSalesAccounts: q.numbers,
		Totals: gen.MarginReportTotals{Revenue: amount(m.Company.Revenue), CostOfSales: amount(m.Company.CostOfSales), Margin: amount(m.Company.Margin)}}
	rows, next := q.page(m)
	if next != nil {
		tok, _ := e.Pages.Encode(q.scope, *next) // a string, a uuid and a flag always encode
		out.NextPageToken.SetTo(tok)
	}
	out.Items = make([]gen.MarginReportRow, 0, len(rows)+1)
	for _, r := range rows {
		item := row(r.MarginAmounts)
		item.ProfitCenterID.SetTo(r.ID)
		item.ProfitCenterCode.SetTo(r.Code)
		item.ProfitCenterName.SetTo(r.Name)
		out.Items = append(out.Items, item)
	}
	if next == nil && (m.Unassigned.Revenue != 0 || m.Unassigned.CostOfSales != 0) {
		item := row(m.Unassigned)
		item.Unassigned = true
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// page is the page's profit centers and where the next page starts, nil
// on the last: after the page's last profit center while more follow, and
// at the tail when the page is full of profit centers and the unassigned
// row is still to come, so no page holds more rows than asked for.
func (q mrQuery) page(m ledger.Margin) ([]ledger.MarginRow, *mrCursor) {
	rows := m.Rows
	if len(rows) > q.size {
		rows = rows[:q.size]
		return rows, &mrCursor{Code: rows[q.size-1].Code, ID: rows[q.size-1].ID}
	}
	if !q.tail && len(rows) == q.size && (m.Unassigned.Revenue != 0 || m.Unassigned.CostOfSales != 0) {
		return rows, &mrCursor{Tail: true}
	}
	return rows, nil
}

// marginQuery resolves the request: the page, the periods, the legal
// entity, answered as missing when it does not exist or the caller may
// not act in it (04 §4.9), and the cost of sales accounts of its chart.
func (e *Endpoints) marginQuery(ctx context.Context, params gen.GetMarginReportParams) (mrQuery, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return mrQuery{}, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return mrQuery{}, err
	}
	from, to, err := periodRange(params.FiscalPeriodFrom, params.FiscalPeriodTo)
	if err != nil {
		return mrQuery{}, err
	}
	q := mrQuery{size: size, scope: fmt.Sprintf("getMarginReport|%s|%d|%d|%d", params.LegalEntityID, params.FiscalYear, from, to),
		margin: ledger.MarginQuery{LegalEntityID: params.LegalEntityID, FiscalYear: int16(params.FiscalYear), PeriodFrom: from, PeriodTo: to, Limit: size + 1}}
	if params.PageToken.Set {
		var c mrCursor
		if err := e.Pages.Decode(q.scope, params.PageToken.Value, &c); err != nil {
			return mrQuery{}, err
		}
		if q.tail = c.Tail; c.Tail {
			q.margin.Limit = 0
		} else {
			q.margin.After = &ledger.MarginKey{Code: c.Code, ID: c.ID}
		}
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return mrQuery{}, err
	}
	if q.le, err = tenancy.Accessible(ctx, e.LegalEntities, access, params.LegalEntityID, "legal_entity_id"); err != nil {
		return mrQuery{}, err
	}
	if q.exp, err = currency.Exponent(ctx, tx, q.le.LocalCurrency, nil); err != nil {
		return mrQuery{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return mrQuery{}, err
	}
	q.numbers, q.named = doc.CostOfSalesAccounts(q.le.Code)
	q.margin.CostOfSales = q.numbers
	return q, nil
}
