package receivables

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// GetReceivablesAging implements GET /v1/receivables-aging: the aging of
// a company's customer open items on a key date (Aging), a page of
// customers and the totals of them all.
func (e *Endpoints) GetReceivablesAging(ctx context.Context, params gen.GetReceivablesAgingParams) (gen.GetReceivablesAgingRes, error) {
	a, err := e.Aging(ctx, AgingQuery{LegalEntity: params.LegalEntityID, KeyDate: params.KeyDate, Days: params.AgingDays,
		Customer: apitypes.Ptr[uuid.UUID](params.CustomerID), PageSize: params.PageSize.Value, PageSizeSet: params.PageSize.Set,
		PageToken: apitypes.Ptr[string](params.PageToken)})
	if err != nil {
		return nil, err
	}
	le := a.Company
	out := gen.ReceivablesAging{LegalEntityID: le.ID, CompanyCode: le.Code, CompanyName: le.Name, KeyDate: a.KeyDate, Currency: le.LocalCurrency,
		AgingDays: a.Days, Totals: gen.ReceivablesAgingTotals{CustomerCount: int64(a.Customers), ItemCount: a.Totals.total.count,
			Amount: apitypes.Money(a.Totals.total.amount, le.LocalCurrency, a.Exponent), Buckets: agingBuckets(a.Totals, a)},
		Items: make([]gen.ReceivablesAgingCustomer, len(a.Page))}
	if a.Next != "" {
		out.NextPageToken.SetTo(a.Next)
	}
	for i, row := range a.Page {
		out.Items[i] = gen.ReceivablesAgingCustomer{CustomerID: row.customer, CustomerNumber: row.Number, CustomerName: row.Name, ItemCount: row.total.count,
			Amount: apitypes.Money(row.total.amount, le.LocalCurrency, a.Exponent), Buckets: agingBuckets(row.aged, a)}
	}
	return &out, nil
}

// agingBuckets are a customer's or the report's buckets as the API shows
// them, every one of them, an empty one with no items and an amount of 0:
// named and bounded as GET /v1/open-items names its aging's
// (ledger.AgingRange).
func agingBuckets(s aged, a Aging) []gen.ReceivablesAgingBucket {
	out := make([]gen.ReceivablesAgingBucket, len(s.buckets))
	for slot, b := range s.buckets {
		bucket := slot
		if slot == len(s.buckets)-1 {
			bucket = -1 // without a due date, shown last
		}
		name, from, to := ledger.AgingRange(bucket, a.Days)
		out[slot] = gen.ReceivablesAgingBucket{Bucket: name, DaysFrom: apitypes.OptInt32(from), DaysTo: apitypes.OptInt32(to), ItemCount: b.count,
			Amount: apitypes.Money(b.amount, a.Company.LocalCurrency, a.Exponent)}
	}
	return out
}
