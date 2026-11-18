package receivables

import (
	"bytes"
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/csvcell"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// GetReceivablesAging implements GET /v1/receivables-aging: the aging of
// a company's customer open items on a key date (Aging), a page of
// customers or of profit centers or segments and the totals of them all,
// as JSON or, with format csv (T75m), as CSV for a spreadsheet.
func (e *Endpoints) GetReceivablesAging(ctx context.Context, params gen.GetReceivablesAgingParams) (gen.GetReceivablesAgingRes, error) {
	format := params.Format.Or("json")
	if format != "json" && format != "csv" {
		return nil, problem.BadRequest("request.invalid_parameter", "Parameter format is json or csv.",
			problem.FieldError{Field: "format", Code: "invalid", Message: "json, csv"})
	}
	a, err := e.Aging(ctx, AgingQuery{LegalEntity: params.LegalEntityID, KeyDate: params.KeyDate, Days: params.AgingDays,
		Customer: apitypes.Ptr[uuid.UUID](params.CustomerID), OpenOn: params.OpenOn.Value, Basis: params.AgingBasis.Value,
		ProfitCenter: apitypes.Ptr[uuid.UUID](params.ProfitCenterID), Segment: apitypes.Ptr[uuid.UUID](params.SegmentID), GroupBy: params.GroupBy.Value,
		PageSize: params.PageSize.Value, PageSizeSet: params.PageSize.Set, PageToken: apitypes.Ptr[string](params.PageToken)})
	if err != nil {
		return nil, err
	}
	next := apitypes.OptText(a.Next)
	if format == "csv" {
		var b bytes.Buffer
		_ = csvcell.Write(&b, agingCSV(a)) // a bytes.Buffer takes every write
		return &gen.GetReceivablesAgingOKTextCsvHeaders{NextPageToken: next, Response: gen.GetReceivablesAgingOKTextCsv{Data: &b}}, nil
	}
	return &gen.ReceivablesAgingHeaders{NextPageToken: next, Response: agingJSON(a)}, nil
}

// agingJSON is the aging as the API's JSON shows it.
func agingJSON(a Aging) gen.ReceivablesAging {
	le := a.Company
	out := gen.ReceivablesAging{LegalEntityID: le.ID, CompanyCode: le.Code, CompanyName: le.Name, KeyDate: a.KeyDate, OpenOn: a.OpenOn, AgingBasis: a.Basis,
		GroupBy: a.GroupBy, ProfitCenterID: apitypes.OptUUID(a.ProfitCenter), SegmentID: apitypes.OptUUID(a.Segment), Currency: le.LocalCurrency,
		AgingDays: a.Days, Totals: gen.ReceivablesAgingTotals{CustomerCount: a.Totals.customers, ItemCount: a.Totals.total.count,
			Amount: apitypes.Money(a.Totals.total.amount, le.LocalCurrency, a.Exponent), Buckets: agingBuckets(a.Totals, a)},
		Items: make([]gen.ReceivablesAgingCustomer, len(a.Page)), NextPageToken: apitypes.OptText(a.Next)}
	for i, row := range a.Page {
		out.Items[i] = gen.ReceivablesAgingCustomer{CustomerID: row.key, CustomerNumber: row.Number, CustomerName: row.Name, ItemCount: row.total.count,
			Amount: apitypes.Money(row.total.amount, le.LocalCurrency, a.Exponent), Buckets: agingBuckets(row.aged, a)}
	}
	if a.GroupBy != byCustomer {
		out.Groups = make([]gen.ReceivablesAgingGroup, len(a.Groups))
		for i, g := range a.Groups {
			out.Groups[i] = agingGroup(g, a)
		}
	}
	return out
}

// agingGroup is a profit center's or a segment's row as the API shows it:
// the center by id, code and name, none of them on the row of the items
// carrying none.
func agingGroup(g agedGroup, a Aging) gen.ReceivablesAgingGroup {
	out := gen.ReceivablesAgingGroup{CustomerCount: g.customers, ItemCount: g.total.count,
		Amount: apitypes.Money(g.total.amount, a.Company.LocalCurrency, a.Exponent), Buckets: agingBuckets(g.aged, a)}
	if g.key == uuid.Nil {
		return out
	}
	if a.GroupBy == byProfitCenter {
		out.ProfitCenterID, out.ProfitCenterCode, out.ProfitCenterName = gen.NewOptUUID(g.key), gen.NewOptString(g.Code), gen.NewOptString(g.Name)
	} else {
		out.SegmentID, out.SegmentCode, out.SegmentName = gen.NewOptUUID(g.key), gen.NewOptString(g.Code), gen.NewOptString(g.Name)
	}
	return out
}

// agingBuckets are a row's or the report's buckets as the API shows
// them, every one of them, an empty one with no items and an amount of 0:
// named and bounded as GET /v1/open-items names its aging's
// (ledger.AgingRange).
func agingBuckets(s aged, a Aging) []gen.ReceivablesAgingBucket {
	out := make([]gen.ReceivablesAgingBucket, len(s.buckets))
	for slot, b := range s.buckets {
		name, from, to := ledger.AgingRange(bucketOf(slot, len(s.buckets)), a.Days)
		out[slot] = gen.ReceivablesAgingBucket{Bucket: name, DaysFrom: apitypes.OptInt32(from), DaysTo: apitypes.OptInt32(to), ItemCount: b.count,
			Amount: apitypes.Money(b.amount, a.Company.LocalCurrency, a.Exponent)}
	}
	return out
}

// bucketOf is the ledger's bucket number of a row's slot: the slot's own,
// but for the last, the bucket without a due date (-1), shown last.
func bucketOf(slot, slots int) int {
	if slot == slots-1 {
		return -1
	}
	return slot
}

// agingCSV is the page as CSV rows (T75m): the header, a row per row of
// the page and, on the last page, the totals' row. A row is its kind, its
// customer's or center's id, number or code and name, the currency, its
// customers, items and amount, then its amount in each bucket, every
// amount an exact decimal.
func agingCSV(a Aging) [][]string {
	slots := len(a.Days) + 3 // as age counts them
	header := []string{"row", "id", "code", "name", "currency", "customer_count", "item_count", "amount"}
	for slot := range slots {
		header = append(header, bucketColumn(bucketOf(slot, slots), a.Days))
	}
	rows := [][]string{header}
	for _, c := range a.Page {
		rows = append(rows, csvRow(a, byCustomer, c.key.String(), c.Number, c.Name, c.aged))
	}
	for _, g := range a.Groups {
		id := ""
		if g.key != uuid.Nil {
			id = g.key.String()
		}
		rows = append(rows, csvRow(a, a.GroupBy, id, g.Code, g.Name, g.aged))
	}
	if a.Next == "" {
		rows = append(rows, csvRow(a, "total", "", "", "", a.Totals))
	}
	return rows
}

// bucketColumn names a bucket's column: not_due, overdue_<from>_<to>,
// the last overdue bucket overdue_<from>_plus, no_due_date.
func bucketColumn(bucket int, days []int32) string {
	name, from, to := ledger.AgingRange(bucket, days)
	switch {
	case from == nil:
		return name
	case to == nil:
		return name + "_" + strconv.Itoa(int(*from)) + "_plus"
	}
	return name + "_" + strconv.Itoa(int(*from)) + "_" + strconv.Itoa(int(*to))
}

// csvRow is one row of the CSV: its cells as agingCSV lists them.
func csvRow(a Aging, kind, id, code, name string, s aged) []string {
	row := []string{kind, id, code, name, a.Company.LocalCurrency, strconv.FormatInt(s.customers, 10), strconv.FormatInt(s.total.count, 10),
		s.total.amount.Plain(a.Exponent)}
	for _, b := range s.buckets {
		row = append(row, b.amount.Plain(a.Exponent))
	}
	return row
}
