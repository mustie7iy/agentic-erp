package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// promiseRefs is what a page of promises is shown with: what they name
// by id (their companies, customers and who recorded them), the company's
// today each status is judged on, by company, and which of their items
// are no longer open.
type promiseRefs struct {
	caseRefs
	today  map[uuid.UUID]time.Time
	closed map[ledger.ItemRef]bool
}

// promiseRefsOf reads what the promises are shown with, once each.
func (e *Endpoints) promiseRefsOf(ctx context.Context, promises []Promise) (promiseRefs, error) {
	les, customers, people := make([]uuid.UUID, len(promises)), make([]uuid.UUID, len(promises)), make([]uuid.UUID, len(promises))
	var items []ledger.ItemRef
	for i, p := range promises {
		les[i], customers[i], people[i] = p.LegalEntityID, p.CustomerID, p.CreatedBy
		for _, it := range p.Items {
			items = append(items, it.ItemRef)
		}
	}
	named, err := e.named(ctx, "promises to pay", les, customers, people)
	if err != nil {
		return promiseRefs{}, err
	}
	out := promiseRefs{caseRefs: named, today: map[uuid.UUID]time.Time{}}
	if len(promises) > 0 {
		doc, _, err := e.Settings.Document(ctx)
		if err != nil {
			return promiseRefs{}, fmt.Errorf("collections: the companies' days of %d promises to pay: %w", len(promises), err)
		}
		now := clock.Now(e.Now)
		for id, le := range named.companies {
			day, err := doc.CompanyDay(le.Code, now)
			if err != nil {
				return promiseRefs{}, fmt.Errorf("collections: company %s's day: %w", le.Code, err)
			}
			out.today[id] = day.Date
		}
	}
	if out.closed, err = e.OpenItems.Closed(ctx, items); err != nil {
		return promiseRefs{}, fmt.Errorf("collections: the items of %d promises to pay: %w", len(promises), err)
	}
	return out, nil
}

// promiseToAPI is a promise as the API gives it: its company by code and
// name, its customer by number and name, who recorded it by name, each
// item with whether it is still open, and its status on the company's
// today.
func promiseToAPI(p Promise, r promiseRefs) gen.PromiseToPay {
	le, today := r.companies[p.LegalEntityID], r.today[p.LegalEntityID]
	p.stillOpen(func(ref ledger.ItemRef) bool { return !r.closed[ref] })
	out := gen.PromiseToPay{ID: p.ID, Version: p.Version, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(p.UpdatedBy), LegalEntityID: p.LegalEntityID, CompanyCode: le.Code, CompanyName: le.Name, CustomerID: p.CustomerID,
		CustomerNumber: p.CustomerNumber, CustomerName: r.customers[p.CustomerID].Name, PromisedAmount: apitypes.Money(p.Amount, p.Currency, p.Exponent),
		PromisedDate: p.Date, Status: p.StatusOn(today), StatusDate: today, PaidAmount: apitypes.Money(p.Paid(), p.Currency, p.Exponent),
		Items: make([]gen.PromiseToPayItem, len(p.Items)), CreatedByName: r.people[p.CreatedBy]}
	for i, it := range p.Items {
		out.Items[i] = gen.PromiseToPayItem{FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo, DocumentNumber: it.DocumentNumber,
			Reference: apitypes.OptString(it.Reference), NetDueDate: it.NetDueDate, Amount: apitypes.Money(it.Amount, it.Currency, it.Exponent),
			LocalAmount: apitypes.Money(it.AmountLC, p.Currency, p.Exponent), Open: it.Open}
	}
	return out
}

// shownPromise is a promise as the API gives it, with what it names.
func (e *Endpoints) shownPromise(ctx context.Context, p Promise) (gen.PromiseToPay, error) {
	r, err := e.promiseRefsOf(ctx, []Promise{p})
	if err != nil {
		return gen.PromiseToPay{}, err
	}
	return promiseToAPI(p, r), nil
}

// CreatePromiseToPay implements POST /v1/promises-to-pay.
func (e *Endpoints) CreatePromiseToPay(ctx context.Context, req *gen.PromiseToPayCreate, _ gen.CreatePromiseToPayParams) (gen.CreatePromiseToPayRes, error) {
	refs := make([]ledger.ItemRef, len(req.Items))
	for i, it := range req.Items {
		// The contract bounds the year to 1900-2100.
		refs[i] = ledger.ItemRef{FiscalYear: int16(it.FiscalYear), EntryID: it.JournalEntryID, LineNo: it.LineNo}
	}
	p, err := e.RecordPromise(ctx, PromiseInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, Amount: req.PromisedAmount.Amount,
		Currency: req.PromisedAmount.Currency, Date: req.PromisedDate, Items: refs})
	if err != nil {
		return nil, err
	}
	out, err := e.shownPromise(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.PromiseToPayHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Location: gen.NewOptString("/v1/promises-to-pay/" + p.ID.String()),
		Response: out}, nil
}

// GetPromiseToPay implements GET /v1/promises-to-pay/{id}.
func (e *Endpoints) GetPromiseToPay(ctx context.Context, params gen.GetPromiseToPayParams) (gen.GetPromiseToPayRes, error) {
	p, err := GetPromise(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.shownPromise(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.GetPromiseToPayOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
}

var promiseSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "p.created_at", Type: "timestamptz"},
}}

// ListPromisesToPay implements GET /v1/promises-to-pay: the promises of
// the companies the caller may act in, the latest first.
func (e *Endpoints) ListPromisesToPay(ctx context.Context, params gen.ListPromisesToPayParams) (gen.ListPromisesToPayRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	ks, err := e.Pages.Keyset(promiseSort, fmt.Sprintf("listPromisesToPay|%s|%s", le, customer), apitypes.Page(params.OrderBy, params.PageSize,
		params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("collections: page of promises to pay: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("collections: company access for promises to pay: %w", err)
	}
	q, args := ks.SQL(promiseSelect+` WHERE ($1 OR p.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR p.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR p.customer_id = $4)`, []any{access.All, access.IDs, le, customer})
	rows, _ := tx.Query(ctx, q, args...)
	promises, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Promise, error) { return scanPromise(r) })
	if err != nil {
		return nil, fmt.Errorf("collections: list promises to pay: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, promises, func(p Promise) (string, uuid.UUID) { return pagination.Instant(p.CreatedAt), p.ID },
		func(page []Promise) (promiseRefs, error) {
			if err := promiseItems(ctx, tx, page); err != nil {
				return promiseRefs{}, err
			}
			return e.promiseRefsOf(ctx, page)
		}, promiseToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.PromiseToPayPage{Items: items, NextPageToken: next}, nil
}
