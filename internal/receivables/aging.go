package receivables

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The AR aging (T75e, ADR-0063 §10): a company's customer open items on a
// key date, per customer and in total, not yet due and in buckets of days
// overdue. It is computed on read and keeps no table: the items are the
// ledger's (ledger.OpenItems.Receivable), so a customer item a manual or
// migrated entry posted counts as an invoice's does, and each item's
// bucket is the one GET /v1/open-items ages it into (ledger.AgingBucket),
// but for an item posted without payment terms: that list shows it
// without a due date, and here it is due on its document date, as for
// dunning and the collection worklist (ledger.ReceivableItem.NetDueDate;
// product owner 2026-11-12), so every item has a due date and the report's
// last bucket, no_due_date, is always empty: it is still given, since
// taking a value out of the response would break its consumers.

// The key date's years: the contract's range of fiscal years, so a date
// no period can hold is refused (422) rather than aged. The bound is not
// what keeps the buckets exact: ledger.ReceivableItem.DaysOverdue counts
// through a time.Duration, which holds about 292 years and stops there in
// either direction (time.Time.Sub), and with a bucket's last day at most
// 3650 (the contract's) 292 years overdue is past every bucket, as any
// longer time is, and 292 years before the due date is not due.
const minKeyYear, maxKeyYear = 1900, 2999

// OpenItems reads a company's open customer items (ledger context).
type OpenItems interface {
	Receivable(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID) ([]ledger.ReceivableItem, error)
}

// agingSum is the items of a bucket, a customer or the report: how many,
// and their amount in the company's currency, credits negative.
type agingSum struct {
	count  int64
	amount money.Minor
}

// add counts an item of the amount; a sum out of range is
// money.ErrOverflow.
func (s *agingSum) add(amount money.Minor) error {
	sum, err := money.Sum(s.amount, amount)
	if err != nil {
		return err
	}
	s.count, s.amount = s.count+1, sum
	return nil
}

// aged is a customer's open items on the key date, or the report's: all
// of them, and by bucket in the order the report shows them: not due,
// each overdue bucket by its days (ledger.AgingBucket's numbers), and
// last the bucket without a due date, which nothing falls in.
type aged struct {
	customer uuid.UUID
	total    agingSum
	buckets  []agingSum
}

// age sums the items per customer and in total on the key date, each item
// in exactly one bucket by its net due date (ledger.AgingBucket). The
// items come ordered by customer, as ledger.OpenItems.Receivable returns
// them, and the customers keep that order. Sums out of range are refused
// (422 receivables.amount_too_large).
func age(items []ledger.ReceivableItem, key time.Time, days []int32) ([]aged, aged, error) {
	slots := len(days) + 3 // not due, an overdue bucket per day and one past the last, no due date (always empty)
	totals := aged{buckets: make([]agingSum, slots)}
	var rows []aged
	for _, it := range items {
		if len(rows) == 0 || rows[len(rows)-1].customer != it.PartnerID {
			rows = append(rows, aged{customer: it.PartnerID, buckets: make([]agingSum, slots)})
		}
		slot := ledger.AgingBucket(it.DaysOverdue(key), true, days) // a customer item always has a due date
		row := &rows[len(rows)-1]
		for _, s := range []*agingSum{&row.total, &row.buckets[slot], &totals.total, &totals.buckets[slot]} {
			if err := s.add(it.AmountLC); err != nil { // money.Sum's only error is the range
				return nil, aged{}, problem.Unprocessable("receivables.amount_too_large", "The open items' amounts add up out of range.")
			}
		}
	}
	return rows, totals, nil
}

// agingCursor is where a page of the aging ends: its last customer.
type agingCursor struct {
	After uuid.UUID `json:"after"`
}

// agingRequest is a request for the aging that passed validation.
type agingRequest struct {
	le       tenancy.Entity
	key      time.Time
	days     []int32
	customer *uuid.UUID
	size     int
	scope    string
	after    *uuid.UUID
}

// agingRequest checks the request: a company the caller may act in, a key
// date in the years 1900 to 2999 (422 receivables.key_date_out_of_range
// naming key_date), ascending bucket days (the default without any), a
// page size and a page token of this report with these parameters.
func (e *Endpoints) agingRequest(ctx context.Context, params gen.GetReceivablesAgingParams) (agingRequest, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return agingRequest{}, fmt.Errorf("receivables: company access for the aging: %w", err)
	}
	r := agingRequest{key: params.KeyDate, customer: apitypes.Ptr[uuid.UUID](params.CustomerID)}
	if r.le, err = tenancy.Accessible(ctx, e.LegalEntities, access, params.LegalEntityID, "legal_entity_id"); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: legal entity of the aging: %w", err)
	}
	if y := r.key.Year(); y < minKeyYear || y > maxKeyYear {
		return agingRequest{}, problem.Invalid("receivables.key_date_out_of_range", "key_date",
			fmt.Sprintf("The key date is in the years %d to %d.", minKeyYear, maxKeyYear))
	}
	if r.days, err = ledger.AgingDays(params.AgingDays); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: buckets of the aging: %w", err)
	}
	if r.size, err = pagination.Size(params.PageSize.Value, params.PageSize.Set); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
	}
	only := "" // every customer
	if r.customer != nil {
		only = r.customer.String()
	}
	r.scope = fmt.Sprintf("getReceivablesAging|%s|%s|%v|%s", r.le.ID, r.key.Format(time.DateOnly), r.days, only)
	if params.PageToken.Set {
		var c agingCursor
		if err := e.Pages.Decode(r.scope, params.PageToken.Value, &c); err != nil {
			return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
		}
		r.after = &c.After
	}
	return r, nil
}

// GetReceivablesAging implements GET /v1/receivables-aging: the aging of
// a company's customer open items on a key date, a page of customers and
// the totals of them all.
func (e *Endpoints) GetReceivablesAging(ctx context.Context, params gen.GetReceivablesAgingParams) (gen.GetReceivablesAgingRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := e.agingRequest(ctx, params)
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, r.le.LocalCurrency, nil) // a company's currency is a known one
	if err != nil {
		return nil, fmt.Errorf("receivables: currency of company %s: %w", r.le.Code, err)
	}
	var customers []uuid.UUID
	if r.customer != nil {
		customers = []uuid.UUID{*r.customer}
	}
	items, err := e.OpenItems.Receivable(ctx, r.le.ID, customers)
	if err != nil {
		return nil, fmt.Errorf("receivables: open customer items of company %s: %w", r.le.Code, err)
	}
	rows, totals, err := age(items, r.key, r.days)
	if err != nil {
		return nil, err
	}
	out := gen.ReceivablesAging{LegalEntityID: r.le.ID, CompanyCode: r.le.Code, CompanyName: r.le.Name, KeyDate: r.key, Currency: r.le.LocalCurrency,
		AgingDays: r.days, Totals: gen.ReceivablesAgingTotals{CustomerCount: int64(len(rows)), ItemCount: totals.total.count,
			Amount: apitypes.Money(totals.total.amount, r.le.LocalCurrency, exp), Buckets: agingBuckets(totals, r, exp)}}
	page, next := e.agingPage(rows, r)
	out.NextPageToken = next
	ids := make([]uuid.UUID, len(page))
	for i, row := range page {
		ids[i] = row.customer
	}
	names, err := e.Partners.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("receivables: customers of the aging of company %s: %w", r.le.Code, err)
	}
	out.Items = make([]gen.ReceivablesAgingCustomer, len(page))
	for i, row := range page {
		out.Items[i] = gen.ReceivablesAgingCustomer{CustomerID: row.customer, CustomerNumber: names[row.customer].Number,
			CustomerName: names[row.customer].Name, ItemCount: row.total.count, Amount: apitypes.Money(row.total.amount, r.le.LocalCurrency, exp),
			Buckets: agingBuckets(row, r, exp)}
	}
	return &out, nil
}

// agingPage is the page of customers after the request's cursor, in the
// rows' order (by customer id, the order the ledger reads them in), and
// the token of the next page when customers are left.
func (e *Endpoints) agingPage(rows []aged, r agingRequest) ([]aged, gen.OptString) {
	if r.after != nil {
		i, _ := slices.BinarySearchFunc(rows, *r.after, func(row aged, after uuid.UUID) int {
			if c := bytes.Compare(row.customer[:], after[:]); c != 0 {
				return c
			}
			return -1 // the cursor's customer closed the page before
		})
		rows = rows[i:]
	}
	if len(rows) <= r.size {
		return rows, gen.OptString{}
	}
	token, _ := e.Pages.Encode(r.scope, agingCursor{After: rows[r.size-1].customer}) // a uuid always encodes
	return rows[:r.size], gen.NewOptString(token)
}

// agingBuckets are a customer's or the report's buckets as the API shows
// them, every one of them, an empty one with no items and an amount of 0:
// named and bounded as GET /v1/open-items names its aging's
// (ledger.AgingRange).
func agingBuckets(a aged, r agingRequest, exp int) []gen.ReceivablesAgingBucket {
	out := make([]gen.ReceivablesAgingBucket, len(a.buckets))
	for slot, s := range a.buckets {
		bucket := slot
		if slot == len(a.buckets)-1 {
			bucket = -1 // without a due date, shown last
		}
		name, from, to := ledger.AgingRange(bucket, r.days)
		out[slot] = gen.ReceivablesAgingBucket{Bucket: name, DaysFrom: apitypes.OptInt32(from), DaysTo: apitypes.OptInt32(to), ItemCount: s.count,
			Amount: apitypes.Money(s.amount, r.le.LocalCurrency, exp)}
	}
	return out
}
