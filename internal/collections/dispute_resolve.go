package collections

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The resolutions of an accepted dispute case (T86d): a customer credit
// memo per accepted item, or a write-off of the accepted amounts.
const (
	CreditMemo = "credit_memo"
	WriteOff   = "write_off"
)

// WriteOffKey is the account determination key a write-off debits: the
// payment differences account an underpayment within the tolerance is
// written off to (T75f), as SAP posts a dispute case's write-off by
// payment difference reason (transaction key ZDI; from memory).
const WriteOffKey = "payment_difference_expense"

// WriteOffDocumentType is the journal document type of a write-off: a
// general document, as the ledger's manual entries (SA).
const WriteOffDocumentType = "SA"

// Resolving is what resolving a dispute case posts through (T86d;
// ADR-0065 As built): receivables' published API, which enters and posts a
// customer credit memo in the request's transaction as sales' credit memo
// requests do (ADR-0067 As built T85d), and the ledger's posting port, its
// account determination and the source documents of the items' entries.
type Resolving struct {
	Invoices Invoices
	Postings Postings
	Accounts Determination
	Sources  Sources
}

// Invoices is receivables' published API for customer invoices and credit
// memos.
type Invoices interface {
	Invoice(ctx context.Context, id uuid.UUID) (receivables.Invoice, error)
	CreateInvoice(ctx context.Context, in receivables.Input) (receivables.Invoice, error)
	PostInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (receivables.Invoice, error)
}

// Postings is the ledger's posting port.
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
}

// Determination reads a company's account determination (ledger context).
type Determination interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
}

// Sources names the documents a context posted entries for (ledger
// context): an item's customer invoice.
type Sources interface {
	Sources(ctx context.Context, sourceContext string, entries []ledger.EntryRef) (map[ledger.EntryRef]uuid.UUID, error)
}

// Resolution is how an accepted case was resolved: Kind, CreditMemo or
// WriteOff; what it accepted in the case's currency and, as its items were
// booked, in the company's; the date its documents posted on; and a
// write-off's journal entry, nil for a credit memo resolution, whose items
// name their memos.
type Resolution struct {
	Kind                 string
	Accepted, AcceptedLC money.Minor
	PostingDate          time.Time
	WriteOff             *EntryRef
}

// EntryRef names a journal entry by fiscal year, id and document number.
type EntryRef struct {
	Year   int16
	ID     uuid.UUID
	Number string
}

// resolutionRow is a case's resolution columns as scanned, all null for a
// case not resolved (dispute_case_resolved_check).
type resolutionRow struct {
	kind                 *string
	accepted, acceptedLC *money.Minor
	postingDate          *time.Time
	entryYear            *int16
	entryID              *uuid.UUID
	entryNumber          *string
}

func (r resolutionRow) resolution() *Resolution {
	if r.kind == nil {
		return nil
	}
	out := &Resolution{Kind: *r.kind, Accepted: *r.accepted, AcceptedLC: *r.acceptedLC, PostingDate: *r.postingDate}
	if r.entryID != nil { // a write-off names its entry (dispute_case_write_off_check)
		out.WriteOff = &EntryRef{Year: *r.entryYear, ID: *r.entryID, Number: *r.entryNumber}
	}
	return out
}

// ResolutionInput is a resolution as a client asks for it: its kind, the
// reason, the posting date (nil: the company's today) and what is accepted
// of which items (none: every item in full).
type ResolutionInput struct {
	Kind        string
	Reason      string
	PostingDate *time.Time
	Items       []AcceptedInput
}

// AcceptedInput is what is accepted of a case's item, by its number: a
// decimal string in the case's currency.
type AcceptedInput struct {
	ItemNo           int32
	Amount, Currency string
}

// resolved is what a resolution accepted of each item, in the case's
// currency and in the company's, and the documents it posted: a credit
// memo per item accepted by one, or the write-off's entry.
type resolved struct {
	accepted, acceptedLC []money.Minor
	memos                []*receivables.Invoice
	entry                *ledger.Entry
	date                 time.Time
}

// ResolveCase resolves an open case the company accepts, in full or in
// part, once, with the caller's version of it (ADR-0065 As built, T86d):
// the case is read under its row lock; a credit memo resolution enters and
// posts a customer credit memo per item accepted above zero through
// receivables, a write-off posts one entry through the ledger; the items'
// accepted amounts and documents and the case, closed as accepted with
// its resolution, are written by updates guarded by its being open
// (ADR-0008), with the audit record and DisputeCaseChanged, all in the
// request's transaction (ADR-0007, ADR-0011). A refusal by receivables or
// the ledger leaves the case open and posts nothing.
func (e *Endpoints) ResolveCase(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in ResolutionInput) (Case, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Case{}, err
	}
	if in.Kind != CreditMemo && in.Kind != WriteOff {
		return Case{}, problem.Invalid("collections.unknown_resolution", "resolution", fmt.Sprintf("%q is no resolution of a dispute case; "+
			"the resolutions are %s and %s.", in.Kind, CreditMemo, WriteOff))
	}
	if err := texts(map[string]*string{"reason": &in.Reason}); err != nil {
		return Case{}, err
	}
	c, err := lockOpen(ctx, tx, id, ifMatch)
	if err != nil {
		return Case{}, err
	}
	r, err := acceptedOf(c, in.Items)
	if err != nil {
		return Case{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, c.LegalEntityID) // a stored case's legal entity exists
	if err != nil {
		return Case{}, fmt.Errorf("collections: company of dispute case %s: %w", id, err)
	}
	if r.date, err = e.resolutionDate(ctx, le, in.PostingDate); err != nil {
		return Case{}, err
	}
	if in.Kind == CreditMemo {
		r.memos, err = e.creditMemos(ctx, c, r)
	} else {
		r.entry, err = e.writeOff(ctx, le, c, r)
	}
	if err != nil {
		return Case{}, err
	}
	if err := markResolved(ctx, tx, p, c, in, r); err != nil {
		return Case{}, err
	}
	return e.changed(ctx, tx, id, "collections.dispute_case.resolve", "resolved", map[string]any{"outcome": Accepted, "reason_given": true})
}

// acceptedOf reads what is accepted of each of the case's items: in full
// when the request names none; otherwise what it names, by item number
// (each once), in the case's currency, from zero to what the item
// disputes, the items it does not name not accepted. Something must be
// accepted: a case the company does not accept is closed as rejected.
func acceptedOf(c Case, in []AcceptedInput) (resolved, error) {
	r := resolved{accepted: make([]money.Minor, len(c.Items)), acceptedLC: make([]money.Minor, len(c.Items))}
	if len(in) == 0 {
		for i, it := range c.Items {
			r.accepted[i], r.acceptedLC[i] = it.Amount, it.AmountLC
		}
		return r, nil
	}
	seen := make(map[int32]bool, len(in))
	for k, a := range in {
		field := fmt.Sprintf("items[%d]", k)
		if a.ItemNo < 1 || int(a.ItemNo) > len(c.Items) || seen[a.ItemNo] {
			return resolved{}, problem.Invalid("collections.unknown_item", field+".item_no", fmt.Sprintf("Item %d is no item of the case named once; "+
				"its items are numbered 1 to %d.", a.ItemNo, len(c.Items)))
		}
		seen[a.ItemNo] = true
		it := c.Items[a.ItemNo-1]
		if a.Currency != c.Currency {
			return resolved{}, problem.Invalid("collections.currency_mismatch", field+".accepted_amount.currency", "The accepted amount is in the "+
				"case's currency, "+c.Currency+".")
		}
		m, err := currency.ParseAmount(a.Amount, a.Currency, c.Exponent, field+".accepted_amount.amount")
		if err != nil {
			return resolved{}, err
		}
		if m < 0 || m > it.Amount {
			return resolved{}, problem.Invalid("collections.invalid_accepted_amount", field+".accepted_amount.amount", fmt.Sprintf("What is accepted "+
				"of item %d is from zero to what it disputes, %s %s.", it.ItemNo, it.Amount.Plain(c.Exponent), c.Currency))
		}
		r.accepted[a.ItemNo-1], r.acceptedLC[a.ItemNo-1] = m, shareOf(it.AmountLC, m, it.Amount)
	}
	if !slices.ContainsFunc(r.accepted, func(m money.Minor) bool { return m > 0 }) {
		return resolved{}, problem.Invalid("collections.invalid_accepted_amount", "items", "A resolution accepts more than zero of the case; "+
			"a case the company does not accept is closed as rejected (closeDisputeCase).")
	}
	return r, nil
}

// resolutionDate is the date the resolution's documents post on: the
// request's, else the company's today in its time zone.
func (e *Endpoints) resolutionDate(ctx context.Context, le tenancy.Entity, requested *time.Time) (time.Time, error) {
	if requested != nil {
		return *requested, nil
	}
	today, err := e.today(ctx, le)
	return today.Date, err
}

// creditMemos enters and posts a customer credit memo for each item
// accepted above zero, naming the item's customer invoice: the posted
// customer invoice its entry was posted for (ledger source documents,
// receivables context); an item of any other entry is refused, 422
// collections.no_invoice. The memos are returned by item, nil for an item
// not credited.
func (e *Endpoints) creditMemos(ctx context.Context, c Case, r resolved) ([]*receivables.Invoice, error) {
	var refs []ledger.EntryRef
	for i, it := range c.Items {
		if r.accepted[i] > 0 {
			refs = append(refs, ledger.EntryRef{FiscalYear: it.FiscalYear, EntryID: it.EntryID})
		}
	}
	sources, err := e.Resolving.Sources.Sources(ctx, "receivables", refs)
	if err != nil {
		return nil, fmt.Errorf("collections: the documents of dispute case %s's items: %w", c.ID, err)
	}
	out := make([]*receivables.Invoice, len(c.Items))
	for i, it := range c.Items {
		if r.accepted[i] == 0 {
			continue
		}
		inv, err := e.invoiceOf(ctx, it, sources)
		if err != nil {
			return nil, err
		}
		memo, err := e.creditMemo(ctx, c, it, inv, r.accepted[i], r.date)
		if err != nil {
			return nil, err
		}
		out[i] = &memo
	}
	return out, nil
}

// invoiceOf is the posted customer invoice the item's entry was posted
// for; any other item is refused, 422 collections.no_invoice.
func (e *Endpoints) invoiceOf(ctx context.Context, it CaseItem, sources map[ledger.EntryRef]uuid.UUID) (receivables.Invoice, error) {
	refused := problem.Invalid("collections.no_invoice", "resolution", fmt.Sprintf("Item %d, entry %s, is no posted customer invoice of "+
		"receivables, which a credit memo would name; resolve the case by a write-off, or close it and credit the item by hand.", it.ItemNo,
		it.DocumentNumber))
	id, ok := sources[ledger.EntryRef{FiscalYear: it.FiscalYear, EntryID: it.EntryID}]
	if !ok {
		return receivables.Invoice{}, refused
	}
	inv, err := e.Resolving.Invoices.Invoice(ctx, id)
	var p *problem.Problem
	switch {
	case errors.As(err, &p) && p.Status == 404: // an incoming payment's residual item, for one: not a customer invoice
		return receivables.Invoice{}, refused
	case err != nil:
		return receivables.Invoice{}, fmt.Errorf("collections: the customer invoice of item %d: %w", it.ItemNo, err)
	case inv.Kind != receivables.KindInvoice || inv.Status != receivables.Posted:
		return receivables.Invoice{}, refused
	}
	return inv, nil
}

// creditMemo enters and posts the customer credit memo of an item: the
// case's company and customer, the invoice's currency, named, dated and
// posted on date, its items the invoice's in proportion to the accepted
// part of its gross amount (creditedItems), and the case's item as its
// source, by the case's id and the item's number. It keeps the invoice's
// term (ADR-0063 §7). A refusal names the resolution (asResolution).
func (e *Endpoints) creditMemo(ctx context.Context, c Case, it CaseItem, inv receivables.Invoice, accepted money.Minor,
	date time.Time) (receivables.Invoice, error) {
	lead := fmt.Sprintf("Item %d's credit memo, crediting %s %s of customer invoice %s, cannot be posted: ", it.ItemNo,
		accepted.Plain(inv.Exponent), inv.Currency, inv.DocumentNumber)
	in := receivables.Input{LegalEntityID: c.LegalEntityID, CustomerID: c.CustomerID, Kind: receivables.KindCreditMemo, ReferenceInvoiceID: &inv.ID,
		InvoiceDate: date, PostingDate: &date, Currency: inv.Currency, Items: creditedItems(inv, accepted),
		Source: &receivables.Source{ID: it.ID, Number: fmt.Sprintf("%s/%d", c.ID, it.ItemNo)}} // 36 + 1 + 2 characters, within 40
	draft, err := e.Resolving.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		err = fmt.Errorf("collections: enter the credit memo of dispute case %s item %d: %w", c.ID, it.ItemNo, err)
		return receivables.Invoice{}, asResolution(err, lead)
	}
	// The draft is this transaction's: no one else can have read it, so it posts at the version it was entered at.
	v, err := e.Resolving.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, &date)
	if err != nil {
		err = fmt.Errorf("collections: post credit memo %s of dispute case %s: %w", draft.DocumentNumber, c.ID, err)
		return receivables.Invoice{}, asResolution(err, lead)
	}
	return v, nil
}

// creditedItems are the invoice's items credited in proportion to the
// accepted part of its gross amount: the accepted amount shared out over
// the items by their gross amounts, and each item's share over its net
// amount and its tax by theirs (money.Allocate, so each level adds up
// exactly), the tax given as the share of what the item bore, rounded
// once per memo (ADR-0049 §8; ADR-0065 As built T86d). An item whose share
// is zero is left out.
func creditedItems(inv receivables.Invoice, accepted money.Minor) []receivables.ItemInput {
	grosses := make([]*big.Rat, len(inv.Items))
	for i, it := range inv.Items {
		grosses[i] = ratio(it.Net+it.Tax, accepted, inv.Gross)
	}
	shares, _ := money.Allocate(grosses) // the shares add up to the accepted amount, at most the invoice's gross: in range
	var out []receivables.ItemInput
	for i, it := range inv.Items {
		if shares[i] == 0 {
			continue
		}
		// The parts add up to the item's share: in range.
		parts, _ := money.Allocate([]*big.Rat{ratio(it.Net, shares[i], it.Net+it.Tax), ratio(it.Tax, shares[i], it.Net+it.Tax)})
		tax := parts[1]
		out = append(out, receivables.ItemInput{Description: it.Description, AccountID: it.AccountID,
			Net: receivables.Amount{Value: parts[0].Plain(inv.Exponent), Currency: inv.Currency}, TaxCode: it.TaxCode, TaxJurisdiction: it.TaxJurisdiction,
			CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID, Tax: &tax})
	}
	return out
}

// writeOff posts the write-off of the accepted amounts: one entry of
// document type SA, source context collections, the case its document,
// with per item accepted above zero a debit on the company's
// payment_difference_expense account and a credit to the item's account
// with the customer, both at the item's booked rate (the accepted part of
// its local amount), so each pair balances in every currency and the
// credit clears against the item without an exchange difference. A
// company without the account is refused, 422
// collections.no_write_off_account; a refusal by the ledger names the
// resolution (asResolution).
func (e *Endpoints) writeOff(ctx context.Context, le tenancy.Entity, c Case, r resolved) (*ledger.Entry, error) {
	account, ok, err := e.Resolving.Accounts.Determination(ctx, le.ID, WriteOffKey)
	if err != nil {
		return nil, fmt.Errorf("collections: the write-off account of company %s: %w", le.Code, err)
	}
	if !ok {
		return nil, problem.Unprocessable("collections.no_write_off_account", "A write-off is posted to the company's "+WriteOffKey+
			" account, and company "+le.Code+" has none in its account determination.")
	}
	var lines []ledger.PostingLine
	for i, it := range c.Items {
		if r.accepted[i] == 0 {
			continue
		}
		text, customer, lc, back := "Dispute write-off "+it.DocumentNumber, c.CustomerID, r.acceptedLC[i], -r.acceptedLC[i]
		lines = append(lines, ledger.PostingLine{AccountID: account, Amount: r.accepted[i], LC: &lc, Text: &text},
			ledger.PostingLine{AccountID: it.AccountID, Amount: -r.accepted[i], LC: &back, PartnerID: &customer, Text: &text})
	}
	header := "Write-off of dispute case " + c.ID.String()
	entry, err := e.Resolving.Postings.Post(ctx, ledger.Posting{LegalEntityID: le.ID, DocumentType: WriteOffDocumentType, DocumentDate: r.date,
		PostingDate: r.date, Currency: c.Currency, HeaderText: &header, SourceContext: "collections", SourceDocumentID: c.ID, Lines: lines})
	if err != nil {
		return nil, asResolution(fmt.Errorf("collections: post the write-off of dispute case %s: %w", c.ID, err), "The write-off cannot be posted: ")
	}
	return &entry, nil
}

// asResolution names a refusal by receivables or the ledger as the
// resolution's: its detail led by what could not be posted, each field it
// names but the posting date the request's resolution, whose documents
// the consumer did not name field by field. Any other error is returned as
// it is.
func asResolution(err error, lead string) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	refused := *p
	refused.Detail = lead + p.Detail
	refused.Errors = slices.Clone(p.Errors)
	for k := range refused.Errors {
		if refused.Errors[k].Field != "posting_date" {
			refused.Errors[k].Field = "resolution"
		}
	}
	return &refused
}

// markResolved writes what was accepted of each item and its credit memo,
// then closes the case as accepted with its resolution, by updates guarded
// by its being open; dispute_case_items holds them to each other at
// commit.
func markResolved(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Case, in ResolutionInput, r resolved) error {
	nos, memoIDs, memoNumbers := make([]int16, len(c.Items)), make([]*uuid.UUID, len(c.Items)), make([]*string, len(c.Items))
	for i, it := range c.Items {
		nos[i] = it.ItemNo
		if r.memos != nil && r.memos[i] != nil {
			memoIDs[i], memoNumbers[i] = &r.memos[i].ID, &r.memos[i].DocumentNumber
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE collections.dispute_case_item i SET accepted_amount = x.a, accepted_amount_lc = x.lc, credit_memo_id = x.m,
			credit_memo_number = x.n
		FROM unnest($2::smallint[], $3::bigint[], $4::bigint[], $5::uuid[], $6::text[]) AS x(no, a, lc, m, n)
		WHERE i.dispute_case_id = $1 AND i.item_no = x.no AND i.accepted_amount IS NULL`,
		c.ID, nos, r.accepted, r.acceptedLC, memoIDs, memoNumbers); err != nil {
		return fmt.Errorf("collections: the accepted items of dispute case %s: %w", c.ID, err)
	}
	return closeResolved(ctx, tx, p, c, in, r)
}

// closeResolved closes the case as accepted with its resolution, guarded
// by its being open.
func closeResolved(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c Case, in ResolutionInput, r resolved) error {
	// Each item's accepted amounts are at most what it disputes, which add up to the case's amounts: the sums are in range.
	accepted, _ := money.Sum(r.accepted...)
	lc, _ := money.Sum(r.acceptedLC...)
	var year *int16
	var entry *uuid.UUID
	var number *string
	if r.entry != nil {
		year, entry, number = &r.entry.FiscalYear, &r.entry.ID, &r.entry.DocumentNumber
	}
	if _, err := tx.Exec(ctx, `UPDATE collections.dispute_case SET status = 'accepted', close_reason = $2, closed_at = now(), closed_by = $3,
			resolution = $4, accepted_amount = $5, accepted_amount_lc = $6, posting_date = $7, write_off_entry_year = $8, write_off_entry_id = $9,
			write_off_entry_number = $10, version = version + 1, updated_by = $3
		WHERE id = $1 AND status = 'open'`, c.ID, in.Reason, p.ID, in.Kind, accepted, lc, r.date, year, entry, number); err != nil {
		return fmt.Errorf("collections: resolve dispute case %s: %w", c.ID, err)
	}
	return nil
}
