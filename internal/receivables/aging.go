package receivables

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
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
//
// Two variants (T75l): the items open on the key date rather than now
// (open_on key_date, ledger.OpenItems.ReceivableOn: the report as it
// stood that day, an item paid since aged as open), and the days counted
// from each item's document date rather than its net due date
// (aging_basis document_date), as if every item were due the day it is
// dated.

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
	// ReceivableOn reads the customer items open on a date (T75l).
	ReceivableOn(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID, on time.Time) ([]ledger.ReceivableItem, error)
	// Closed names the lines no longer open, without a lock (T75g: a
	// payment's credit on account).
	Closed(ctx context.Context, refs []ledger.ItemRef) (map[ledger.ItemRef]bool, error)
	// ClearedApart names the ledger's clearings of a line of the entry or
	// of the lines against an item that is neither (relievedclearing.go).
	ClearedApart(ctx context.Context, entryYear int16, entryID uuid.UUID, lines []ledger.ItemRef) ([]ledger.ClearingRef, error)
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

// aged is a row's open items on the key date, or the report's: all of
// them, and by bucket in the order the report shows them: not due, each
// overdue bucket by its days (ledger.AgingBucket's numbers), and last the
// bucket without a due date, which nothing falls in. key is the row's
// customer, or its profit center or segment (uuid.Nil for the items
// carrying none, T75m); customers counts the customers of its items, last
// the one counted last.
type aged struct {
	key       uuid.UUID
	customers int64
	last      uuid.UUID
	total     agingSum
	buckets   []agingSum
}

// count adds the item to the row's customers when its customer is not the
// one counted last: the items come ordered by customer, so each customer's
// items follow one another in every row they fall in.
func (a *aged) count(customer uuid.UUID) {
	if a.customers == 0 || a.last != customer {
		a.customers, a.last = a.customers+1, customer
	}
}

// The values of open_on and aging_basis (T75l) and of group_by (T75m),
// the first of each the default.
const (
	openNow           = "now"
	openOnKeyDate     = "key_date"
	basisNetDueDate   = "net_due_date"
	basisDocumentDate = "document_date"
	byCustomer        = "customer"
	byProfitCenter    = "profit_center"
	bySegment         = "segment"
)

// groupKey is the row an item falls in by the grouping: its customer, or
// its line's profit center or segment, uuid.Nil for none.
func groupKey(it ledger.ReceivableItem, groupBy string) uuid.UUID {
	var center *uuid.UUID
	switch groupBy {
	case byCustomer:
		return it.PartnerID
	case byProfitCenter:
		center = it.ProfitCenterID
	default: // bySegment, the only other value agingRequest lets through
		center = it.SegmentID
	}
	if center == nil {
		return uuid.Nil
	}
	return *center
}

// selected reports whether the item carries the profit center and the
// segment the request names (T75m), each nil for any.
func selected(it ledger.ReceivableItem, profitCenter, segment *uuid.UUID) bool {
	same := func(want, got *uuid.UUID) bool { return want == nil || got != nil && *got == *want }
	return same(profitCenter, it.ProfitCenterID) && same(segment, it.SegmentID)
}

// daysPast is how many days past the date the basis names the item is on
// the key date: its net due date (ledger.ReceivableItem.DaysOverdue) or
// its document date.
func daysPast(it ledger.ReceivableItem, key time.Time, basis string) int {
	if basis == basisDocumentDate {
		return ledger.DaysPast(key, it.DocumentDate)
	}
	return it.DaysOverdue(key)
}

// age sums the request's items per row and in total on the key date, each
// item in exactly one bucket by its days past the date the basis names
// (ledger.AgingBucket) and in exactly one row by the grouping (groupKey).
// The items come ordered by customer, as ledger.OpenItems.Receivable
// returns them; the rows are ordered by key, as the page's cursor reads
// them, which for customers is that order, and the row without a center
// (uuid.Nil) first. Sums out of range are refused (422
// receivables.amount_too_large).
func age(items []ledger.ReceivableItem, r agingRequest) ([]aged, aged, error) {
	slots := len(r.days) + 3 // not due, an overdue bucket per day and one past the last, no due date (always empty)
	totals := aged{buckets: make([]agingSum, slots)}
	var rows []aged
	at := map[uuid.UUID]int{}
	for _, it := range items {
		if !selected(it, r.profitCenter, r.segment) {
			continue
		}
		key := groupKey(it, r.groupBy)
		i, ok := at[key]
		if !ok {
			i, at[key] = len(rows), len(rows)
			rows = append(rows, aged{key: key, buckets: make([]agingSum, slots)})
		}
		slot := ledger.AgingBucket(daysPast(it, r.key, r.basis), true, r.days) // a customer item always has a due date and a document date
		row := &rows[i]
		row.count(it.PartnerID)
		totals.count(it.PartnerID)
		for _, s := range []*agingSum{&row.total, &row.buckets[slot], &totals.total, &totals.buckets[slot]} {
			if err := s.add(it.AmountLC); err != nil { // money.Sum's only error is the range
				return nil, aged{}, problem.Unprocessable("receivables.amount_too_large", "The open items' amounts add up out of range.")
			}
		}
	}
	slices.SortFunc(rows, func(a, b aged) int { return bytes.Compare(a.key[:], b.key[:]) })
	return rows, totals, nil
}

// agingCursor is where a page of the aging ends: its last row's key.
type agingCursor struct {
	After uuid.UUID `json:"after"`
}

// AgingQuery asks for the aging: the company, the key date, the buckets'
// last days (the default without any), one customer or every one, the
// items open now or on the key date (OpenOn, "" for the default) and the
// date their days are counted from (Basis, "" for the default), and the
// page: its size (PageSizeSet false for the default) and token.
type AgingQuery struct {
	LegalEntity uuid.UUID
	KeyDate     time.Time
	Days        []int32
	Customer    *uuid.UUID
	OpenOn      string
	Basis       string
	// ProfitCenter and Segment select the items carrying them, nil for
	// any; GroupBy is what a row is, "" for the default (T75m).
	ProfitCenter *uuid.UUID
	Segment      *uuid.UUID
	GroupBy      string
	PageSize     int32
	PageSizeSet  bool
	PageToken    *string
}

// Aging is the aging of a company's customer open items on a key date: a
// page of rows, each named, the totals of them all, the token of the next
// page ("" for none), and the buckets' days and the company's currency
// exponent the amounts are shown with, the items it ages (OpenOn), the
// date their days are counted from (Basis), the centers they were
// selected by and what a row is (T75m): with GroupBy customer the page is
// Page, else Groups.
type Aging struct {
	Company      tenancy.Entity
	KeyDate      time.Time
	OpenOn       string
	Basis        string
	GroupBy      string
	ProfitCenter *uuid.UUID
	Segment      *uuid.UUID
	Days         []int32
	Exponent     int
	Totals       aged
	Page         []agedCustomer
	Groups       []agedGroup
	Next         string
}

// agedCustomer is a customer's row on a page, with its number and name.
type agedCustomer struct {
	aged
	partner.Named
}

// agedGroup is a profit center's or a segment's row on a page, with its
// code and name; the row of the items carrying none has its key uuid.Nil
// and neither.
type agedGroup struct {
	aged
	ledger.CenterName
}

// agingRequest is a request for the aging that passed validation.
type agingRequest struct {
	le           tenancy.Entity
	key          time.Time
	days         []int32
	customer     *uuid.UUID
	profitCenter *uuid.UUID
	segment      *uuid.UUID
	openOn       string
	basis        string
	groupBy      string
	size         int
	scope        string
	after        *uuid.UUID
}

// agingRequest checks the request: a company the caller may act in, a key
// date in the years 1900 to 2999 (422 receivables.key_date_out_of_range
// naming key_date), the choices among the contract's values
// (agingChoices), ascending bucket days (the default without any), a page
// size and a page token of this report with these parameters.
func (e *Endpoints) agingRequest(ctx context.Context, q AgingQuery) (agingRequest, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return agingRequest{}, fmt.Errorf("receivables: company access for the aging: %w", err)
	}
	r := agingRequest{key: q.KeyDate, customer: q.Customer, profitCenter: q.ProfitCenter, segment: q.Segment, openOn: cmp.Or(q.OpenOn, openNow),
		basis: cmp.Or(q.Basis, basisNetDueDate), groupBy: cmp.Or(q.GroupBy, byCustomer)}
	if r.le, err = tenancy.Accessible(ctx, e.LegalEntities, access, q.LegalEntity, "legal_entity_id"); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: legal entity of the aging: %w", err)
	}
	if y := r.key.Year(); y < minKeyYear || y > maxKeyYear {
		return agingRequest{}, problem.Invalid("receivables.key_date_out_of_range", "key_date",
			fmt.Sprintf("The key date is in the years %d to %d.", minKeyYear, maxKeyYear))
	}
	if err := agingChoices(r); err != nil {
		return agingRequest{}, err
	}
	if r.days, err = ledger.AgingDays(q.Days); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: buckets of the aging: %w", err)
	}
	if r.size, err = pagination.Size(q.PageSize, q.PageSizeSet); err != nil {
		return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
	}
	r.scope = fmt.Sprintf("getReceivablesAging|%s|%s|%v|%s|%s|%s|%s|%s|%s", r.le.ID, r.key.Format(time.DateOnly), r.days, idText(r.customer),
		r.openOn, r.basis, r.groupBy, idText(r.profitCenter), idText(r.segment))
	if q.PageToken != nil {
		var c agingCursor
		if err := e.Pages.Decode(r.scope, *q.PageToken, &c); err != nil {
			return agingRequest{}, fmt.Errorf("receivables: page of the aging: %w", err)
		}
		r.after = &c.After
	}
	return r, nil
}

// idText is a filter's id as a page token's scope holds it, "" for none.
func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// agingChoices checks open_on, aging_basis and group_by against the
// contract's values (400 request.invalid_parameter naming the field), as
// getAssetHistorySheet checks its group_by.
func agingChoices(r agingRequest) error {
	for _, c := range []struct {
		field, value string
		allowed      []string
	}{
		{"open_on", r.openOn, []string{openNow, openOnKeyDate}},
		{"aging_basis", r.basis, []string{basisNetDueDate, basisDocumentDate}},
		{"group_by", r.groupBy, []string{byCustomer, byProfitCenter, bySegment}},
	} {
		if !slices.Contains(c.allowed, c.value) {
			allowed := strings.Join(c.allowed, ", ")
			return problem.BadRequest("request.invalid_parameter", fmt.Sprintf("Parameter %s is one of %s.", c.field, allowed),
				problem.FieldError{Field: c.field, Code: "invalid", Message: allowed})
		}
	}
	return nil
}

// Aging ages a company's customer open items on a key date: a page of
// rows and the totals of them all.
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
	items, err := e.agedItems(ctx, r, customers)
	if err != nil {
		return Aging{}, fmt.Errorf("receivables: open customer items of company %s: %w", r.le.Code, err)
	}
	rows, totals, err := age(items, r)
	if err != nil {
		return Aging{}, err
	}
	page, next := e.agingPage(rows, r)
	out := Aging{Company: r.le, KeyDate: r.key, OpenOn: r.openOn, Basis: r.basis, GroupBy: r.groupBy, ProfitCenter: r.profitCenter, Segment: r.segment,
		Days: r.days, Exponent: exp, Totals: totals, Next: next}
	if err := e.nameRows(ctx, &out, page); err != nil {
		return Aging{}, fmt.Errorf("receivables: rows of the aging of company %s: %w", r.le.Code, err)
	}
	return out, nil
}

// nameRows puts the page's rows on the aging with their names: the
// customers' numbers and names (partner context), or the profit centers'
// or segments' codes and names (ledger context), the row without a center
// named by neither.
func (e *Endpoints) nameRows(ctx context.Context, a *Aging, page []aged) error {
	ids := make([]uuid.UUID, 0, len(page))
	for _, row := range page {
		if row.key != uuid.Nil {
			ids = append(ids, row.key)
		}
	}
	if a.GroupBy == byCustomer {
		names, err := e.Partners.Names(ctx, ids)
		if err != nil {
			return err
		}
		a.Page = make([]agedCustomer, len(page))
		for i, row := range page {
			a.Page[i] = agedCustomer{aged: row, Named: names[row.key]}
		}
		return nil
	}
	names, err := e.References.CenterNames(ctx, ids)
	if err != nil {
		return err
	}
	a.Groups = make([]agedGroup, len(page))
	for i, row := range page {
		a.Groups[i] = agedGroup{aged: row, CenterName: names[row.key]}
	}
	return nil
}

// agedItems are the customer items the request ages: those open now, or
// those open on the key date (T75l).
func (e *Endpoints) agedItems(ctx context.Context, r agingRequest, customers []uuid.UUID) ([]ledger.ReceivableItem, error) {
	if r.openOn == openOnKeyDate {
		return e.OpenItems.ReceivableOn(ctx, r.le.ID, customers, r.key)
	}
	return e.OpenItems.Receivable(ctx, r.le.ID, customers)
}

// agingPage is the page of rows after the request's cursor, in the rows'
// order (by key: the customer's id, the order the ledger reads them in, or
// the center's, the row without one first), and the token of the next
// page when rows are left ("" otherwise).
func (e *Endpoints) agingPage(rows []aged, r agingRequest) ([]aged, string) {
	if r.after != nil {
		i, _ := slices.BinarySearchFunc(rows, *r.after, func(row aged, after uuid.UUID) int {
			if c := bytes.Compare(row.key[:], after[:]); c != 0 {
				return c
			}
			return -1 // the cursor's row closed the page before
		})
		rows = rows[i:]
	}
	if len(rows) <= r.size {
		return rows, ""
	}
	token, _ := e.Pages.Encode(r.scope, agingCursor{After: rows[r.size-1].key}) // a uuid always encodes
	return rows[:r.size], token
}
