package receivables

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
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

// AgingQuery asks for the aging: the company, the key date, the buckets'
// last days (the default without any), one customer or every one, and the
// page: its size (PageSizeSet false for the default) and token.
type AgingQuery struct {
	LegalEntity uuid.UUID
	KeyDate     time.Time
	Days        []int32
	Customer    *uuid.UUID
	PageSize    int32
	PageSizeSet bool
	PageToken   *string
}

// Aging is the aging of a company's customer open items on a key date: a
// page of customers, each named, the totals of them all, the token of the
// next page ("" for none), and the buckets' days and the company's
// currency exponent the amounts are shown with.
type Aging struct {
	Company   tenancy.Entity
	KeyDate   time.Time
	Days      []int32
	Exponent  int
	Totals    aged
	Customers int // how many customers have open items, on every page
	Page      []agedCustomer
	Next      string
}

// agedCustomer is a customer's row on a page, with its number and name.
type agedCustomer struct {
	aged
	partner.Named
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
func (e *Endpoints) agingRequest(ctx context.Context, q AgingQuery) (agingRequest, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return agingRequest{}, fmt.Errorf("receivables: company access for the aging: %w", err)
	}
	r := agingRequest{key: q.KeyDate, customer: q.Customer}
	if r.le, err = tenancy.Accessible(ctx, e.LegalEntities, access, q.LegalEntity, "legal_entity_id"); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: legal entity of the aging: %w", err)
	}
	if y := r.key.Year(); y < minKeyYear || y > maxKeyYear {
		return agingRequest{}, problem.Invalid("receivables.key_date_out_of_range", "key_date",
			fmt.Sprintf("The key date is in the years %d to %d.", minKeyYear, maxKeyYear))
	}
	if r.days, err = ledger.AgingDays(q.Days); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: buckets of the aging: %w", err)
	}
	if r.size, err = pagination.Size(q.PageSize, q.PageSizeSet); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
	}
	only := "" // every customer
	if r.customer != nil {
		only = r.customer.String()
	}
	r.scope = fmt.Sprintf("getReceivablesAging|%s|%s|%v|%s", r.le.ID, r.key.Format(time.DateOnly), r.days, only)
	if q.PageToken != nil {
		var c agingCursor
		if err := e.Pages.Decode(r.scope, *q.PageToken, &c); err != nil {
			return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
		}
		r.after = &c.After
	}
	return r, nil
}

// Aging ages a company's customer open items on a key date: a page of
// customers and the totals of them all.
func (e *Endpoints) Aging(ctx context.Context, q AgingQuery) (Aging, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Aging{}, err
	}
	r, err := e.agingRequest(ctx, q)
	if err != nil {
		return Aging{}, err
	}
	exp, err := currency.Exponent(ctx, tx, r.le.LocalCurrency, nil) // a company's currency is a known one
	if err != nil {
		return Aging{}, fmt.Errorf("receivables: currency of company %s: %w", r.le.Code, err)
	}
	var customers []uuid.UUID
	if r.customer != nil {
		customers = []uuid.UUID{*r.customer}
	}
	items, err := e.OpenItems.Receivable(ctx, r.le.ID, customers)
	if err != nil {
		return Aging{}, fmt.Errorf("receivables: open customer items of company %s: %w", r.le.Code, err)
	}
	rows, totals, err := age(items, r.key, r.days)
	if err != nil {
		return Aging{}, err
	}
	page, next := e.agingPage(rows, r)
	ids := make([]uuid.UUID, len(page))
	for i, row := range page {
		ids[i] = row.customer
	}
	names, err := e.Partners.Names(ctx, ids)
	if err != nil {
		return Aging{}, fmt.Errorf("receivables: customers of the aging of company %s: %w", r.le.Code, err)
	}
	out := Aging{Company: r.le, KeyDate: r.key, Days: r.days, Exponent: exp, Totals: totals, Customers: len(rows), Next: next,
		Page: make([]agedCustomer, len(page))}
	for i, row := range page {
		out.Page[i] = agedCustomer{aged: row, Named: names[row.customer]}
	}
	return out, nil
}

// agingPage is the page of customers after the request's cursor, in the
// rows' order (by customer id, the order the ledger reads them in), and
// the token of the next page when customers are left ("" otherwise).
func (e *Endpoints) agingPage(rows []aged, r agingRequest) ([]aged, string) {
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
		return rows, ""
	}
	token, _ := e.Pages.Encode(r.scope, agingCursor{After: rows[r.size-1].customer}) // a uuid always encodes
	return rows[:r.size], token
}
