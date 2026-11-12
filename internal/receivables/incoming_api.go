package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// paymentVersion is the version of an incoming payment: it is written once
// and never changed, so its ETag never moves.
const paymentVersion = 1

// paymentReferences are what incoming payments name by id, as their
// consumer reads them (REVIEW.md, API-consumer classes): the companies,
// the customers, the receipts' accounts, and the journal entries (the
// receipt, the clearing entry, the items') by document number and, for
// the items, by reference.
type paymentReferences struct {
	companies  map[uuid.UUID]tenancy.Entity
	partners   map[uuid.UUID]partner.Named
	accounts   map[uuid.UUID]ledger.AccountRef
	numbers    map[uuid.UUID]string
	references map[uuid.UUID]string
}

// paymentReferences reads what the payments name by id, once for all of
// them.
func (e *Endpoints) paymentReferences(ctx context.Context, payments []IncomingPayment) (paymentReferences, error) {
	refs := paymentReferences{companies: map[uuid.UUID]tenancy.Entity{}}
	var partners, accounts, entries, items []uuid.UUID
	for _, v := range payments {
		partners, accounts = append(partners, v.CustomerID), append(accounts, v.ReceiptAccountID)
		entries = append(entries, v.ReceiptEntryID, v.EntryID)
		for _, it := range v.Items {
			items = append(items, it.EntryID)
		}
		if _, ok := refs.companies[v.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored payment's legal entity exists
		if err != nil {
			return paymentReferences{}, fmt.Errorf("receivables: company of incoming payment %s: %w", v.ID, err)
		}
		refs.companies[v.LegalEntityID] = le
	}
	var err error
	if refs.partners, err = e.Partners.Names(ctx, partners); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: customers of %d incoming payments: %w", len(payments), err)
	}
	if refs.accounts, err = e.References.AccountNames(ctx, accounts); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: accounts of %d incoming payments: %w", len(payments), err)
	}
	if refs.numbers, err = e.Entries.Numbers(ctx, append(entries, items...)); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: journal entries of %d incoming payments: %w", len(payments), err)
	}
	if refs.references, err = e.Entries.References(ctx, items); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: references of the items of %d incoming payments: %w", len(payments), err)
	}
	return refs, nil
}

// paymentToAPI is an incoming payment as the API gives it, with what it
// names by id. It never changes, so it was last changed when it was
// created, by whoever created it.
func paymentToAPI(v IncomingPayment, refs paymentReferences) gen.IncomingPayment {
	le, customer, account := refs.companies[v.LegalEntityID], refs.partners[v.CustomerID], refs.accounts[v.ReceiptAccountID]
	out := gen.IncomingPayment{ID: v.ID, Version: paymentVersion, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, UpdatedAt: v.CreatedAt,
		UpdatedBy: gen.NewOptUUID(v.CreatedBy), DocumentNumber: v.DocumentNumber, LegalEntityID: v.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: v.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, PostingDate: v.PostingDate,
		Currency: v.Currency, Amount: apitypes.Money(v.Amount, v.Currency, v.Exponent), ReceiptJournalEntryID: v.ReceiptEntryID,
		ReceiptJournalEntryYear: int32(v.ReceiptYear), ReceiptJournalEntryNumber: refs.numbers[v.ReceiptEntryID], ReceiptAccountID: v.ReceiptAccountID,
		ReceiptAccountNumber: account.Number, ReceiptAccountName: account.Name, JournalEntryID: v.EntryID, JournalEntryYear: int32(v.EntryYear),
		JournalEntryNumber: refs.numbers[v.EntryID], Items: make([]gen.IncomingPaymentItem, len(v.Items))}
	for i, it := range v.Items {
		out.Items[i] = gen.IncomingPaymentItem{ItemNo: it.ItemNo, FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo,
			JournalEntryNumber: refs.numbers[it.EntryID], Reference: refs.references[it.EntryID], Amount: apitypes.Money(it.Amount, v.Currency, v.Exponent)}
	}
	return out
}

// CreateIncomingPayment implements POST /v1/incoming-payments.
func (e *Endpoints) CreateIncomingPayment(ctx context.Context, req *gen.IncomingPaymentCreate, _ gen.CreateIncomingPaymentParams) (gen.CreateIncomingPaymentRes, error) {
	in := PaymentInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, ReceiptEntryID: req.ReceiptJournalEntryID,
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), Items: make([]ledger.ItemRef, len(req.Items))}
	for i, it := range req.Items {
		in.Items[i] = ledger.ItemRef{FiscalYear: int16(it.FiscalYear), EntryID: it.JournalEntryID, LineNo: it.LineNo} // the contract bounds fiscal_year to 1900..2999
	}
	v, err := e.ApplyPayment(ctx, in)
	if err != nil {
		return nil, err
	}
	refs, err := e.paymentReferences(ctx, []IncomingPayment{v})
	if err != nil {
		return nil, err
	}
	return &gen.IncomingPaymentHeaders{ETag: gen.NewOptString(etag.Format(paymentVersion)), Location: gen.NewOptString("/v1/incoming-payments/" + v.ID.String()),
		Response: paymentToAPI(v, refs)}, nil
}

// GetIncomingPayment implements GET /v1/incoming-payments/{id}.
func (e *Endpoints) GetIncomingPayment(ctx context.Context, params gen.GetIncomingPaymentParams) (gen.GetIncomingPaymentRes, error) {
	v, err := GetPayment(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	refs, err := e.paymentReferences(ctx, []IncomingPayment{v})
	if err != nil {
		return nil, err
	}
	return &gen.GetIncomingPaymentOKHeaders{ETag: gen.NewOptString(etag.Format(paymentVersion)), Response: paymentToAPI(v, refs)}, nil
}

var paymentSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "p.posting_date", Type: "date"}, {Name: "document_number", Column: "p.document_number", Type: "text"},
}}

// ListIncomingPayments implements GET /v1/incoming-payments: the payments
// applied in the companies the caller may act in.
func (e *Endpoints) ListIncomingPayments(ctx context.Context, params gen.ListIncomingPaymentsParams) (gen.ListIncomingPaymentsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, customer := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.CustomerID)
	from, to := apitypes.Ptr[time.Time](params.PostingDateFrom), apitypes.Ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listIncomingPayments|%s|%s|%s|%v|%v", le, customer, params.DocumentNumber.Value, from, to)
	ks, err := e.Pages.Keyset(paymentSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("receivables: page of incoming payments: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: company access for incoming payments: %w", err)
	}
	q, args := ks.SQL(paymentSelect+` WHERE ($1 OR p.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR p.legal_entity_id = $3)
		AND ($4::uuid IS NULL OR p.customer_id = $4) AND ($5 = '' OR p.document_number = $5)
		AND ($6::date IS NULL OR p.posting_date >= $6) AND ($7::date IS NULL OR p.posting_date <= $7)`,
		[]any{access.All, access.IDs, le, customer, params.DocumentNumber.Value, from, to})
	rows, _ := tx.Query(ctx, q, args...)
	payments, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IncomingPayment, error) { return scanPayment(r) })
	if err != nil {
		return nil, fmt.Errorf("receivables: list incoming payments: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, payments, func(v IncomingPayment) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return v.DocumentNumber, v.ID
		}
		return v.PostingDate.Format(time.DateOnly), v.ID
	}, func(page []IncomingPayment) (paymentReferences, error) { return e.paymentReferences(ctx, page) }, paymentToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.IncomingPaymentPage{Items: items, NextPageToken: next}, nil
}

// ListUnappliedReceipts implements GET /v1/unapplied-receipts: the
// receipts waiting on the incoming payments clearing accounts of the
// company's house banks, which the caller must be allowed to act in.
func (e *Endpoints) ListUnappliedReceipts(ctx context.Context, params gen.ListUnappliedReceiptsParams) (gen.ListUnappliedReceiptsRes, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: company access for receipts: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, params.LegalEntityID, "legal_entity_id")
	if err != nil {
		return nil, fmt.Errorf("receivables: legal entity of the receipts: %w", err)
	}
	ks, err := e.Pages.Keyset(ledger.ReceiptSort, "listUnappliedReceipts|"+le.ID.String(), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("receivables: page of receipts: %w", err)
	}
	banks, err := e.incomingAccounts(ctx, le)
	if err != nil {
		return nil, err
	}
	receipts, err := e.Receipts.Receipts(ctx, ks, le.ID, accountIDs(banks), nil)
	if err != nil {
		return nil, fmt.Errorf("receivables: receipts of company %s: %w", le.Code, err)
	}
	items, next, err := apitypes.PageWith(ks, receipts, func(r ledger.Receipt) (string, uuid.UUID) {
		return r.PostingDate.Format(time.DateOnly), r.Item.EntryID
	}, func(page []ledger.Receipt) (map[uuid.UUID]ledger.AccountRef, error) {
		ids := make([]uuid.UUID, len(page))
		for i, r := range page {
			ids[i] = r.AccountID
		}
		names, err := e.References.AccountNames(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("receivables: accounts of %d receipts: %w", len(page), err)
		}
		return names, nil
	}, func(r ledger.Receipt, accounts map[uuid.UUID]ledger.AccountRef) gen.UnappliedReceipt {
		return gen.UnappliedReceipt{JournalEntryID: r.Item.EntryID, JournalEntryYear: int32(r.Item.FiscalYear), JournalEntryNumber: r.Number,
			Text: apitypes.OptString(r.HeaderText), LegalEntityID: le.ID, PostingDate: r.PostingDate, Amount: apitypes.Money(r.Amount, r.Currency, r.Exponent),
			AccountID: r.AccountID, AccountNumber: accounts[r.AccountID].Number, AccountName: accounts[r.AccountID].Name,
			HouseBankCode: apitypes.OptText(banks[r.AccountID]), StatementMatchID: apitypes.OptUUID(r.SourceDocumentID)}
	})
	if err != nil {
		return nil, err
	}
	return &gen.UnappliedReceiptPage{Items: items, NextPageToken: next}, nil
}
