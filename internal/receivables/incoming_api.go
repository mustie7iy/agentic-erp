package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// paymentReferences are what incoming payments name by id, as their
// consumer reads them (REVIEW.md, API-consumer classes): the companies,
// the customers, the receipts' and the write-offs' accounts, the journal
// entries (the
// receipt, the clearing entry, the items') by document number and, for
// the items, by reference, and the bank statement line that posted each
// receipt.
type paymentReferences struct {
	companies  map[uuid.UUID]tenancy.Entity
	partners   map[uuid.UUID]partner.Named
	accounts   map[uuid.UUID]ledger.AccountRef
	numbers    map[uuid.UUID]string
	references map[uuid.UUID]string
	statements map[uuid.UUID]StatementLine
	// closed are the payments' credits on account no longer open.
	closed map[ledger.ItemRef]bool
	// parts are each payment's customers with their parts (T75h).
	parts map[uuid.UUID][]CustomerPart
}

// itemIDs appends what the payment's items name by id: their entries,
// their customers and the accounts their reasons wrote off to (T75o).
func (v IncomingPayment) itemIDs(entries, customers, accounts []uuid.UUID) (e, c, a []uuid.UUID) {
	for _, it := range v.Items {
		entries, customers = append(entries, it.EntryID), append(customers, it.CustomerID)
		if it.WriteOffAccount != nil {
			accounts = append(accounts, *it.WriteOffAccount)
		}
	}
	return entries, customers, accounts
}

// paymentReferences reads what the payments name by id, once for all of
// them.
func (e *Endpoints) paymentReferences(ctx context.Context, payments []IncomingPayment) (paymentReferences, error) {
	refs := paymentReferences{companies: map[uuid.UUID]tenancy.Entity{}, parts: map[uuid.UUID][]CustomerPart{}}
	var partners, accounts, entries, items []uuid.UUID
	var receipts, credits []ledger.ItemRef
	for _, v := range payments {
		partners, accounts = append(partners, v.CustomerID), append(accounts, v.ReceiptAccountID)
		entries, receipts = append(entries, v.ReceiptEntryID, v.EntryID), append(receipts, ledger.ItemRef{FiscalYear: v.ReceiptYear, EntryID: v.ReceiptEntryID})
		for _, a := range v.Applications {
			entries = append(entries, a.EntryID)
		}
		for _, r := range v.Resets {
			entries = append(entries, r.ReversalID)
		}
		if credit, _, ok := v.credit(); ok {
			credits = append(credits, credit)
		}
		items, partners, accounts = v.itemIDs(items, partners, accounts)
		refs.parts[v.ID] = v.Parts()
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
	if refs.statements, err = e.Statements.Lines(ctx, receipts); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: statement lines of the receipts of %d incoming payments: %w", len(payments), err)
	}
	if refs.closed, err = e.OpenItems.Closed(ctx, credits); err != nil {
		return paymentReferences{}, fmt.Errorf("receivables: credits on account of %d incoming payments: %w", len(payments), err)
	}
	return refs, nil
}

// paymentToAPI is an incoming payment as the API gives it, with what it
// names by id: its version and last change are its applications' and
// undoings' (version).
func paymentToAPI(v IncomingPayment, refs paymentReferences) gen.IncomingPayment {
	le, customer, account := refs.companies[v.LegalEntityID], refs.partners[v.CustomerID], refs.accounts[v.ReceiptAccountID]
	version, updatedAt, updatedBy := v.version()
	out := gen.IncomingPayment{ID: v.ID, Version: version, CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, UpdatedAt: updatedAt,
		UpdatedBy: gen.NewOptUUID(updatedBy), DocumentNumber: v.DocumentNumber, Status: v.Status(), Reset: resetToAPI(v.resetOf(0), refs),
		LegalEntityID: v.LegalEntityID, CompanyCode: le.Code,
		CompanyName: le.Name, CustomerID: v.CustomerID, CustomerNumber: customer.Number, CustomerName: customer.Name, PostingDate: v.PostingDate,
		Currency: v.Currency, Amount: apitypes.Money(v.Amount, v.Currency, v.Exponent), Difference: apitypes.Money(v.Difference, v.Currency, v.Exponent),
		CashDiscount:            apitypes.Money(v.CashDiscount(), v.Currency, v.Exponent),
		ReceiptJournalEntryID:   v.ReceiptEntryID,
		ReceiptJournalEntryYear: int32(v.ReceiptYear), ReceiptJournalEntryNumber: refs.numbers[v.ReceiptEntryID], ReceiptAccountID: v.ReceiptAccountID,
		ReceiptAccountNumber: account.Number, ReceiptAccountName: account.Name, JournalEntryID: v.EntryID, JournalEntryYear: int32(v.EntryYear),
		JournalEntryNumber: refs.numbers[v.EntryID], Items: make([]gen.IncomingPaymentItem, len(v.Items)),
		Applications: make([]gen.IncomingPaymentApplication, len(v.Applications)), Customers: make([]gen.IncomingPaymentCustomer, len(refs.parts[v.ID]))}
	for i, c := range refs.parts[v.ID] {
		named := refs.partners[c.CustomerID]
		out.Customers[i] = gen.IncomingPaymentCustomer{CustomerID: c.CustomerID, CustomerNumber: named.Number, CustomerName: named.Name,
			Amount: apitypes.Money(c.Amount, v.Currency, v.Exponent)}
	}
	if line, ok := refs.statements[v.ReceiptEntryID]; ok {
		out.ReceiptBankStatementID.SetTo(line.StatementID)
		out.ReceiptBankStatementRef.SetTo(line.StatementRef)
		out.ReceiptBankStatementLineNo.SetTo(line.LineNo)
	}
	for i, it := range v.Items {
		out.Items[i] = paymentItemToAPI(v, it, refs)
	}
	amount := func(m money.Minor) gen.Money { return apitypes.Money(m, v.Currency, v.Exponent) }
	for i, a := range v.Applications {
		day, _ := time.Parse(time.DateOnly, a.PostingDate) // a date column, as json_build_object writes it
		out.Applications[i] = gen.IncomingPaymentApplication{ApplicationNo: a.No, PostingDate: day, Amount: amount(a.Amount), Difference: amount(a.Difference),
			OnAccountAmount: amount(a.OnAccount), JournalEntryID: a.EntryID, JournalEntryYear: int32(a.EntryYear), JournalEntryNumber: refs.numbers[a.EntryID],
			CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy, Reset: resetToAPI(v.resetOf(a.No), refs)}
	}
	if v.OnAccount != 0 {
		on := gen.IncomingPaymentOnAccount{Amount: amount(v.OnAccount), OpenAmount: amount(0)}
		if credit, open, ok := v.credit(); ok {
			if !refs.closed[credit] {
				on.OpenAmount = amount(open)
				on.FiscalYear.SetTo(int32(credit.FiscalYear))
				on.JournalEntryID.SetTo(credit.EntryID)
				on.JournalEntryNumber.SetTo(refs.numbers[credit.EntryID])
				on.LineNo.SetTo(credit.LineNo)
			}
		}
		out.OnAccount.SetTo(on)
	}
	return out
}

// paymentItemToAPI is an item of the payment as the API gives it, with
// what it names by id: its translation, residual item, write-off and cash
// discount where it has them.
func paymentItemToAPI(v IncomingPayment, it PaymentItem, refs paymentReferences) gen.IncomingPaymentItem {
	named := refs.partners[it.CustomerID]
	out := gen.IncomingPaymentItem{ItemNo: it.ItemNo, ApplicationNo: it.ApplicationNo, CustomerID: it.CustomerID, CustomerNumber: named.Number,
		CustomerName: named.Name, FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID,
		LineNo: it.LineNo, JournalEntryNumber: refs.numbers[it.EntryID], Reference: refs.references[it.EntryID],
		Amount: apitypes.Money(it.Amount, v.Currency, v.Exponent), ItemAmount: apitypes.Money(it.Amount, v.Currency, v.Exponent)}
	if it.ItemAmount != nil { // stored with its currency and rate (incoming_payment_item_translation)
		out.ItemAmount = apitypes.Money(*it.ItemAmount, *it.ItemCurrency, *it.ItemExponent)
		out.ExchangeRate.SetTo(gen.Decimal(*it.ExchangeRate))
	}
	if it.ResidualAmount != nil { // stored with its line and reason (incoming_payment_item_residual)
		year, entry := v.clearing(it.ApplicationNo)
		out.Residual.SetTo(gen.IncomingPaymentResidual{Amount: apitypes.Money(*it.ResidualAmount, v.Currency, v.Exponent), Reason: *it.ResidualReason,
			ReasonText: apitypes.OptString(it.ResidualText), FiscalYear: int32(year), JournalEntryID: entry, JournalEntryNumber: refs.numbers[entry],
			LineNo: *it.ResidualLineNo})
	}
	if it.WriteOffAmount != nil { // stored with its line, account and reason (incoming_payment_item_write_off)
		out.WriteOff.SetTo(writeOffToAPI(v, it, refs))
	}
	if it.CashDiscount != nil {
		out.CashDiscount.SetTo(apitypes.Money(*it.CashDiscount, v.Currency, v.Exponent))
	}
	return out
}

// writeOffToAPI is what an item's reason wrote off (T75o) as the API gives
// it, with its account's number and name and its clearing entry's number.
func writeOffToAPI(v IncomingPayment, it PaymentItem, refs paymentReferences) gen.IncomingPaymentWriteOff {
	year, entry := v.clearing(it.ApplicationNo)
	account := refs.accounts[*it.WriteOffAccount]
	return gen.IncomingPaymentWriteOff{Amount: apitypes.Money(*it.WriteOffAmount, v.Currency, v.Exponent), Reason: *it.WriteOffReason,
		ReasonText: apitypes.OptString(it.WriteOffText), AccountID: *it.WriteOffAccount, AccountNumber: account.Number, AccountName: account.Name,
		FiscalYear: int32(year), JournalEntryID: entry, JournalEntryNumber: refs.numbers[entry], LineNo: *it.WriteOffLineNo}
}

// resetToAPI is an undoing as the API gives it, with its reversal's
// number; none for nil.
func resetToAPI(r *PaymentReset, refs paymentReferences) gen.OptIncomingPaymentResetDone {
	if r == nil {
		return gen.OptIncomingPaymentResetDone{}
	}
	return gen.NewOptIncomingPaymentResetDone(gen.IncomingPaymentResetDone{Reason: r.Reason, JournalEntryID: r.ReversalID, JournalEntryYear: int32(r.ReversalYear),
		JournalEntryNumber: refs.numbers[r.ReversalID], CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy})
}

// clearing is the clearing entry of the application with the number, the
// payment's own for 0.
func (v IncomingPayment) clearing(application int32) (int16, uuid.UUID) {
	for _, a := range v.Applications {
		if a.No == application {
			return a.EntryYear, a.EntryID
		}
	}
	return v.EntryYear, v.EntryID
}

// CreateIncomingPayment implements POST /v1/incoming-payments.
func (e *Endpoints) CreateIncomingPayment(ctx context.Context, req *gen.IncomingPaymentCreate, _ gen.CreateIncomingPaymentParams) (gen.CreateIncomingPaymentRes, error) {
	in := PaymentInput{LegalEntityID: req.LegalEntityID, CustomerID: req.CustomerID, OtherCustomers: req.OtherCustomerIds, ReceiptEntryID: req.ReceiptJournalEntryID,
		PostingDate: apitypes.Ptr[time.Time](req.PostingDate), OnAccount: req.OnAccount.Or(false)}
	in.Items, in.Residuals, in.CashDiscounts = itemsOf(req.Items)
	v, err := e.ApplyPayment(ctx, in)
	if err != nil {
		return nil, err
	}
	return e.paymentAnswer(ctx, v)
}

// itemsOf reads the items of a request with their residuals, nil for an
// item without one, and whether each takes its cash discount.
func itemsOf(req []gen.IncomingPaymentItemRef) ([]ledger.ItemRef, []*ResidualInput, []bool) {
	items, residuals, discounts := make([]ledger.ItemRef, len(req)), make([]*ResidualInput, len(req)), make([]bool, len(req))
	for i, it := range req {
		items[i] = ledger.ItemRef{FiscalYear: int16(it.FiscalYear), EntryID: it.JournalEntryID, LineNo: it.LineNo} // the contract bounds fiscal_year to 1900..2999
		if r, ok := it.Residual.Get(); ok {
			residuals[i] = &ResidualInput{Value: r.Amount.Amount, Currency: r.Amount.Currency, Reason: r.Reason, Text: apitypes.Ptr[string](r.ReasonText)}
		}
		discounts[i] = it.CashDiscount.Or(false)
	}
	return items, residuals, discounts
}

// paymentAnswer is the payment created or changed, with its ETag and
// where to read it.
func (e *Endpoints) paymentAnswer(ctx context.Context, v IncomingPayment) (*gen.IncomingPaymentHeaders, error) {
	refs, err := e.paymentReferences(ctx, []IncomingPayment{v})
	if err != nil {
		return nil, err
	}
	version, _, _ := v.version()
	return &gen.IncomingPaymentHeaders{ETag: gen.NewOptString(etag.Format(version)), Location: gen.NewOptString("/v1/incoming-payments/" + v.ID.String()),
		Response: paymentToAPI(v, refs)}, nil
}

// ApplyIncomingPaymentOnAccount implements POST
// /v1/incoming-payments/{id}/applications.
func (e *Endpoints) ApplyIncomingPaymentOnAccount(ctx context.Context, req *gen.IncomingPaymentApplicationCreate,
	params gen.ApplyIncomingPaymentOnAccountParams) (gen.ApplyIncomingPaymentOnAccountRes, error) {
	in := CreditInput{PaymentID: params.ID, PostingDate: apitypes.Ptr[time.Time](req.PostingDate)}
	var discounts []bool
	in.Items, in.Residuals, discounts = itemsOf(req.Items)
	if err := noCashDiscount(discounts); err != nil {
		return nil, err
	}
	v, err := e.ApplyOnAccount(ctx, in)
	if err != nil {
		return nil, err
	}
	return e.paymentAnswer(ctx, v)
}

// ResetIncomingPayment implements POST /v1/incoming-payments/{id}:reset.
func (e *Endpoints) ResetIncomingPayment(ctx context.Context, req *gen.IncomingPaymentReset, params gen.ResetIncomingPaymentParams) (gen.ResetIncomingPaymentRes, error) {
	return e.resetAnswer(ctx, ResetInput{PaymentID: params.ID, IfMatch: etag.IfMatch(params.IfMatch), Reason: req.Reason})
}

// ResetIncomingPaymentApplication implements POST
// /v1/incoming-payments/{id}/applications/{application_no}:reset.
func (e *Endpoints) ResetIncomingPaymentApplication(ctx context.Context, req *gen.IncomingPaymentReset,
	params gen.ResetIncomingPaymentApplicationParams) (gen.ResetIncomingPaymentApplicationRes, error) {
	return e.resetAnswer(ctx, ResetInput{PaymentID: params.ID, ApplicationNo: params.ApplicationNo, IfMatch: etag.IfMatch(params.IfMatch), Reason: req.Reason})
}

// resetAnswer undoes the application and answers the payment with its
// ETag.
func (e *Endpoints) resetAnswer(ctx context.Context, in ResetInput) (*gen.ResetIncomingPaymentApplicationOKHeaders, error) {
	v, err := e.Reset(ctx, in)
	if err != nil {
		return nil, err
	}
	refs, err := e.paymentReferences(ctx, []IncomingPayment{v})
	if err != nil {
		return nil, err
	}
	version, _, _ := v.version()
	return &gen.ResetIncomingPaymentApplicationOKHeaders{ETag: gen.NewOptString(etag.Format(version)), Response: paymentToAPI(v, refs)}, nil
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
	version, _, _ := v.version()
	return &gen.GetIncomingPaymentOKHeaders{ETag: gen.NewOptString(etag.Format(version)), Response: paymentToAPI(v, refs)}, nil
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
		AND ($4::uuid IS NULL OR p.customer_id = $4 OR EXISTS (SELECT FROM receivables.incoming_payment_item t
			WHERE t.tenant_id = p.tenant_id AND t.customer_id = $4 AND t.incoming_payment_id = p.id))
		AND ($5 = '' OR p.document_number = $5)
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

// receiptReferences are what receipts name by id: their accounts, and
// the bank statement line that posted each.
type receiptReferences struct {
	accounts   map[uuid.UUID]ledger.AccountRef
	statements map[uuid.UUID]StatementLine
}

// receiptReferences reads what the receipts name by id, once for all of
// them.
func (e *Endpoints) receiptReferences(ctx context.Context, receipts []ledger.Receipt) (receiptReferences, error) {
	accounts, entries := make([]uuid.UUID, len(receipts)), make([]ledger.ItemRef, len(receipts))
	for i, r := range receipts {
		accounts[i], entries[i] = r.AccountID, r.Item
	}
	var refs receiptReferences
	var err error
	if refs.accounts, err = e.References.AccountNames(ctx, accounts); err != nil {
		return receiptReferences{}, fmt.Errorf("receivables: accounts of %d receipts: %w", len(receipts), err)
	}
	if refs.statements, err = e.Statements.Lines(ctx, entries); err != nil {
		return receiptReferences{}, fmt.Errorf("receivables: statement lines of %d receipts: %w", len(receipts), err)
	}
	return refs, nil
}

// ListUnappliedReceipts implements GET /v1/unapplied-receipts: the
// receipts waiting on the incoming payments clearing accounts of the
// company's house banks, which the caller must be allowed to act in,
// filtered by house bank, currency and posting date.
func (e *Endpoints) ListUnappliedReceipts(ctx context.Context, params gen.ListUnappliedReceiptsParams) (gen.ListUnappliedReceiptsRes, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("receivables: company access for receipts: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, params.LegalEntityID, "legal_entity_id")
	if err != nil {
		return nil, fmt.Errorf("receivables: legal entity of the receipts: %w", err)
	}
	f := ledger.ReceiptFilter{Currency: params.Currency.Value, From: apitypes.Ptr[time.Time](params.PostingDateFrom), To: apitypes.Ptr[time.Time](params.PostingDateTo)}
	scope := fmt.Sprintf("listUnappliedReceipts|%s|%s|%s|%v|%v", le.ID, params.HouseBankCode.Value, f.Currency, f.From, f.To)
	ks, err := e.Pages.Keyset(ledger.ReceiptSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("receivables: page of receipts: %w", err)
	}
	banks, err := e.incomingAccounts(ctx, le)
	if err != nil {
		return nil, err
	}
	receipts, err := e.Receipts.Receipts(ctx, ks, le.ID, accountIDs(banks, params.HouseBankCode.Value), f)
	if err != nil {
		return nil, fmt.Errorf("receivables: receipts of company %s: %w", le.Code, err)
	}
	items, next, err := apitypes.PageWith(ks, receipts, func(r ledger.Receipt) (string, uuid.UUID) {
		return r.PostingDate.Format(time.DateOnly), r.Item.EntryID
	}, func(page []ledger.Receipt) (receiptReferences, error) { return e.receiptReferences(ctx, page) },
		func(r ledger.Receipt, refs receiptReferences) gen.UnappliedReceipt {
			out := gen.UnappliedReceipt{JournalEntryID: r.Item.EntryID, JournalEntryYear: int32(r.Item.FiscalYear), JournalEntryNumber: r.Number,
				Text: apitypes.OptString(r.HeaderText), LegalEntityID: le.ID, PostingDate: r.PostingDate, Amount: apitypes.Money(r.Amount, r.Currency, r.Exponent),
				AccountID: r.AccountID, AccountNumber: refs.accounts[r.AccountID].Number, AccountName: refs.accounts[r.AccountID].Name,
				HouseBankCode: banks[r.AccountID]}
			if line, ok := refs.statements[r.Item.EntryID]; ok {
				out.BankStatementID.SetTo(line.StatementID)
				out.BankStatementRef.SetTo(line.StatementRef)
				out.BankStatementLineNo.SetTo(line.LineNo)
			}
			return out
		})
	if err != nil {
		return nil, err
	}
	return &gen.UnappliedReceiptPage{Items: items, NextPageToken: next}, nil
}
