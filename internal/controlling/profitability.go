package controlling

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Net revenue by profitability segment (ADR-0075 §6, T92a): a company's
// gross value billed, discounts, credit memos and net revenue per
// customer and item key over a range of regular periods, from the sales
// context's posted billing documents and credit memos (sales.Orders
// .Segments), with the customers' numbers and names from the partner
// context, and each segment's cost of sales, the cost of goods sold its
// goods issues posted, and its margin (ADR-0083 §6, S2b2).

// Sales is what the profitability report reads from the sales context
// (sales.Orders).
type Sales interface {
	Segments(ctx context.Context, q sales.SegmentQuery) ([]sales.Segment, int, error)
}

// Partners names the report's customers (partner.Payees).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// prCursor is where a page of the profitability report starts: after the
// segment of the customer with the number and id and the item key (none
// when Keyed is false).
type prCursor struct {
	Number string    `json:"n"`
	ID     uuid.UUID `json:"i"`
	Keyed  bool      `json:"k,omitempty"`
	Key    string    `json:"v,omitempty"`
}

// prRow is a segment with its customer's number and name.
type prRow struct {
	sales.Segment
	partner.Named
}

// compare orders the row against a cursor: by the customer's number and
// id, then the row without an item key first, then the key.
func (r prRow) compare(c prCursor) int {
	keyed := r.ItemKey != nil
	key := ""
	if keyed {
		key = *r.ItemKey
	}
	return cmp.Or(cmp.Compare(r.Number, c.Number), slices.Compare(r.CustomerID[:], c.ID[:]), boolCompare(keyed, c.Keyed), cmp.Compare(key, c.Key))
}

func (r prRow) cursor() prCursor {
	c := prCursor{Number: r.Number, ID: r.CustomerID, Keyed: r.ItemKey != nil}
	if c.Keyed {
		c.Key = *r.ItemKey
	}
	return c
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// prQuery is a page's request, resolved.
type prQuery struct {
	le       tenancy.Entity
	exp      int
	from, to ledger.FiscalPeriod
	segments sales.SegmentQuery
	scope    string
	size     int
	after    *prCursor
}

// GetProfitabilityReport implements GET /v1/profitability-report.
func (e *Endpoints) GetProfitabilityReport(ctx context.Context, params gen.GetProfitabilityReportParams) (gen.GetProfitabilityReportRes, error) {
	q, err := e.profitabilityQuery(ctx, params)
	if err != nil {
		return nil, err
	}
	rows, foreign, err := e.profitabilityRows(ctx, q)
	if err != nil {
		return nil, err
	}
	totals, err := profitabilityTotals(rows)
	if err != nil {
		return nil, fmt.Errorf("controlling: profitability of %s: %w", q.le.ID, err)
	}
	amount := func(v money.Minor) gen.Money { return apitypes.Money(v, q.le.LocalCurrency, q.exp) }
	out := &gen.ProfitabilityReport{LegalEntityID: q.le.ID, CustomerID: apitypes.OptUUID(q.segments.Customer), FiscalYear: params.FiscalYear,
		FiscalPeriodFrom: int32(q.from.Number), FiscalPeriodTo: int32(q.to.Number), DateFrom: q.from.Start, DateTo: q.to.End,
		Currency: q.le.LocalCurrency, ForeignCurrencyDocuments: int32(foreign), // a count of documents, far below 2^31
		IncludesCost: true, Totals: gen.ProfitabilityTotals{GrossValue: amount(totals.Gross), Discounts: amount(totals.Discounts),
			CreditMemos: amount(totals.Credits), NetRevenue: amount(totals.Net), CostOfSales: amount(totals.Cost), Margin: amount(totals.Margin)}}
	page, next := q.page(rows)
	if next != nil {
		tok, _ := e.Pages.Encode(q.scope, *next) // strings, a uuid and a flag always encode
		out.NextPageToken.SetTo(tok)
	}
	out.Items = make([]gen.ProfitabilityRow, len(page))
	for i, r := range page {
		out.Items[i] = gen.ProfitabilityRow{CustomerID: r.CustomerID, CustomerNumber: r.Number, CustomerName: r.Name,
			ItemKey: apitypes.OptString(r.ItemKey), GrossValue: amount(r.Gross), Discounts: amount(r.Discounts),
			CreditMemos: amount(r.Credits), NetRevenue: amount(r.Net), CostOfSales: amount(r.Cost), Margin: amount(r.Margin)}
	}
	return out, nil
}

// profitabilityRows are every segment of the selection with its
// customer's number and name, in the report's order, and the count of
// documents in another currency.
func (e *Endpoints) profitabilityRows(ctx context.Context, q prQuery) ([]prRow, int, error) {
	segments, foreign, err := e.Sales.Segments(ctx, q.segments)
	if err != nil {
		return nil, 0, fmt.Errorf("controlling: segments of %s: %w", q.le.ID, err)
	}
	ids := make([]uuid.UUID, 0, len(segments))
	for _, s := range segments {
		ids = append(ids, s.CustomerID)
	}
	names, err := e.Partners.Names(ctx, slices.Compact(ids)) // segments come by customer, so equal ids are adjacent
	if err != nil {
		return nil, 0, fmt.Errorf("controlling: customers of %s: %w", q.le.ID, err)
	}
	rows := make([]prRow, len(segments))
	for i, s := range segments {
		rows[i] = prRow{Segment: s, Named: names[s.CustomerID]}
	}
	slices.SortFunc(rows, func(a, b prRow) int { return a.compare(b.cursor()) })
	return rows, foreign, nil
}

// profitabilityTotals adds up every segment exactly; a sum beyond a
// 64-bit amount fails.
func profitabilityTotals(rows []prRow) (sales.Segment, error) {
	var t sales.Segment
	for _, r := range rows {
		var err error
		for _, f := range []struct{ to, add *money.Minor }{{&t.Gross, &r.Gross}, {&t.Discounts, &r.Discounts}, {&t.Credits, &r.Credits}, {&t.Net, &r.Net},
			{&t.Cost, &r.Cost}, {&t.Margin, &r.Margin}} {
			if *f.to, err = money.Sum(*f.to, *f.add); err != nil {
				return sales.Segment{}, fmt.Errorf("the totals: %w", err)
			}
		}
	}
	return t, nil
}

// page is the page's rows and where the next page starts, nil on the
// last.
func (q prQuery) page(rows []prRow) ([]prRow, *prCursor) {
	if q.after != nil {
		start, found := slices.BinarySearchFunc(rows, *q.after, prRow.compare)
		if found {
			start++
		}
		rows = rows[start:]
	}
	if len(rows) <= q.size {
		return rows, nil
	}
	rows = rows[:q.size]
	next := rows[q.size-1].cursor()
	return rows, &next
}

// profitabilityQuery resolves the request: the page, the legal entity,
// answered as missing when it does not exist or the caller may not act
// in it (04 §4.9), its currency and the regular periods' dates.
func (e *Endpoints) profitabilityQuery(ctx context.Context, params gen.GetProfitabilityReportParams) (prQuery, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return prQuery{}, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return prQuery{}, err
	}
	if params.FiscalPeriodFrom.Set && params.FiscalPeriodTo.Set && params.FiscalPeriodFrom.Value > params.FiscalPeriodTo.Value {
		return prQuery{}, problem.BadRequest("request.invalid_parameter", "fiscal_period_to is before fiscal_period_from.",
			problem.FieldError{Field: "fiscal_period_to", Code: "before_start", Message: "on or after fiscal_period_from"})
	}
	q := prQuery{size: size, scope: fmt.Sprintf("getProfitabilityReport|%s|%d|%d|%d|%s", params.LegalEntityID, params.FiscalYear,
		params.FiscalPeriodFrom.Or(0), params.FiscalPeriodTo.Or(0), params.CustomerID.Or(uuid.Nil))}
	if params.PageToken.Set {
		var c prCursor
		if err := e.Pages.Decode(q.scope, params.PageToken.Value, &c); err != nil {
			return prQuery{}, err
		}
		q.after = &c
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return prQuery{}, err
	}
	if q.le, err = tenancy.Accessible(ctx, e.LegalEntities, access, params.LegalEntityID, "legal_entity_id"); err != nil {
		return prQuery{}, err
	}
	if q.exp, err = currency.Exponent(ctx, tx, q.le.LocalCurrency, nil); err != nil {
		return prQuery{}, err
	}
	if q.from, q.to, err = e.regularRange(ctx, q.le.ID, params); err != nil {
		return prQuery{}, err
	}
	q.segments = sales.SegmentQuery{LegalEntityID: q.le.ID, From: q.from.Start, To: q.to.End, Currency: q.le.LocalCurrency}
	if params.CustomerID.Set {
		q.segments.Customer = &params.CustomerID.Value
	}
	return q, nil
}

// regularRange is the first and the last regular period of the
// selection, the year's first and last by default; a number that is no
// regular period of the year is refused naming its parameter.
func (e *Endpoints) regularRange(ctx context.Context, le uuid.UUID, params gen.GetProfitabilityReportParams) (from, to ledger.FiscalPeriod, err error) {
	periods, err := e.Calendar.Periods(ctx, le, int16(params.FiscalYear))
	if err != nil {
		return from, to, fmt.Errorf("controlling: periods of %s in %d: %w", le, params.FiscalYear, err)
	}
	find := func(n gen.OptInt32, field string, fallback ledger.FiscalPeriod) (ledger.FiscalPeriod, error) {
		if !n.Set {
			return fallback, nil
		}
		return ledger.PeriodNumbered(periods, params.FiscalYear, n.Value, "controlling.period_not_regular", field)
	}
	if from, err = find(params.FiscalPeriodFrom, "fiscal_period_from", periods[0]); err != nil {
		return from, to, err
	}
	to, err = find(params.FiscalPeriodTo, "fiscal_period_to", periods[len(periods)-1])
	return from, to, err
}
