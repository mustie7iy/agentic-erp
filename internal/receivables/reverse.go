package receivables

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Reversal is what reversing a credit memo or debit memo takes: the
// reason, required, and the reversal's posting date, nil for the
// document's own.
type Reversal struct {
	Reason      string
	PostingDate *time.Time
}

// ReverseInvoice reverses a posted credit memo or debit memo once
// (ADR-0063 As built T75k; SAP's FB08, from memory): the ledger posts the
// reversal of its entry (ledger.Postings.Reverse), the original's lines
// with the signs turned, its tax items taken back on the original's tax
// date, and clears the memo's open item against the reversal's. The
// reversal posts on the date asked, else on the memo's own posting date,
// which must be in an open period (the ledger's 409
// ledger.period_not_open, the period's problem) and not before the memo
// posted. The memo becomes reversed, so a credit memo no longer counts
// against its invoice's limit (creditedInvoice counts posted memos alone).
//
// The memo's row is read FOR UPDATE (ADR-0008) and only a posted one is
// reversed, at the version the client read: a second reversal waits for
// the first and then meets another version (412), or, sent at the new
// version, a reversed memo (409 receivables.invalid_status). An
// invoice is not reversed (corrected by a credit memo, ADR-0063 §5), nor
// a memo another context entered from its document (sales'), which that
// context corrects. Whether the memo's item is cleared is read here only
// to refuse with this context's problem; the ledger's reversal checks it
// again under the entry's lock, which an application of the item takes
// too, so a clearing committed meanwhile refuses the reversal there
// (ledger.entry_cleared) and nothing changes. Nothing else locks the
// memo's row: no document names a credit or debit memo by foreign key
// once it is posted, and an application locks only entries.
func (e *Endpoints) ReverseInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in Reversal) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	v, err := get(ctx, tx, id, true)
	if err != nil {
		return Invoice{}, err
	}
	if err := ifMatch(v.Version); err != nil {
		return Invoice{}, err
	}
	return e.reverse(ctx, tx, p, v, in, false)
}

// ReverseSourced reverses a posted debit memo another context entered
// from its document, for that context (T76i): collections, cancelling the
// dunning run whose notice the memo charged. It is ReverseInvoice without
// the version check, which the caller makes on its own document, and
// without the refusal of a memo entered from a source: source must be
// the memo's source document, or it is an error, not a refusal (the
// caller holds the pair it recorded). The memo's row is read FOR UPDATE,
// and what ReverseInvoice refuses is refused alike: a memo that is not
// posted, one whose item is cleared, a date in a period not open.
func (e *Endpoints) ReverseSourced(ctx context.Context, id, source uuid.UUID, in Reversal) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	v, err := get(ctx, tx, id, true)
	if err != nil {
		return Invoice{}, err
	}
	if v.Kind != KindDebitMemo || v.SourceDocumentID == nil || *v.SourceDocumentID != source {
		return Invoice{}, fmt.Errorf("receivables: customer %s %s was not entered from document %s", v.Kind, v.DocumentNumber, source)
	}
	return e.reverse(ctx, tx, p, v, in, true)
}

// reverse reverses the memo, read under its row lock: what is not
// reversible is refused (reversible; a memo entered from a source only
// when sourced, its context's), else the ledger's reversal is posted and
// the memo marked reversed, with its audit record and event.
func (e *Endpoints) reverse(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, in Reversal, sourced bool) (Invoice, error) {
	date, err := e.reversible(ctx, v, in, sourced)
	if err != nil {
		return Invoice{}, err
	}
	text := "Reversal of " + strings.ToLower(documentTypes[v.Kind].text) + v.DocumentNumber
	rev, err := e.Postings.Reverse(ctx, *v.EntryID, &date, &text)
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: reverse %s %s: %w", v.Kind, v.DocumentNumber, err)
	}
	if err := markReversed(ctx, tx, p, v, rev, in.Reason); err != nil {
		return Invoice{}, err
	}
	reversed, err := load(ctx, tx, v.ID, false)
	if err != nil {
		return Invoice{}, err
	}
	return reversed, e.recordReversed(ctx, reversed, rev)
}

// reversible refuses what is not reversed and returns the reversal's
// posting date: only a posted credit memo or debit memo a person entered,
// or, sourced, its context reversing one it entered (409
// receivables.invalid_status, receivables.invoice_not_reversible,
// receivables.entered_from_source), with a reason that is not white
// space alone, on a date not before the memo posted (422
// receivables.reversal_before_posting), whose open item is not cleared
// (409 receivables.document_cleared).
func (e *Endpoints) reversible(ctx context.Context, v Invoice, in Reversal, sourced bool) (time.Time, error) {
	switch {
	case v.Status != Posted:
		return time.Time{}, problem.Conflict("receivables.invalid_status", "Customer document "+v.DocumentNumber+" is "+v.Status+
			"; only a posted credit memo or debit memo is reversed.")
	case v.Kind == KindInvoice:
		return time.Time{}, problem.Conflict("receivables.invoice_not_reversible", "Customer invoice "+v.DocumentNumber+
			" is not reversed: enter a credit memo naming it (POST /v1/customer-invoices with kind credit_memo).")
	case v.SourceDocumentID != nil && !sourced:
		return time.Time{}, problem.Conflict("receivables.entered_from_source", "Customer "+strings.ReplaceAll(v.Kind, "_", " ")+" "+v.DocumentNumber+
			" was entered from "+sourceOf(v))
	}
	if err := problem.RequireText("reason", in.Reason); err != nil {
		return time.Time{}, err
	}
	date := *v.PostingDate // a posted document names its posting date (customer_invoice_posted)
	if in.PostingDate != nil {
		date = *in.PostingDate
	}
	if date.Before(*v.PostingDate) {
		return time.Time{}, problem.Invalid("receivables.reversal_before_posting", "posting_date",
			"The reversal posts on or after "+v.PostingDate.Format(time.DateOnly)+", the date "+v.DocumentNumber+" posted on.")
	}
	cleared, err := e.Postings.IsCleared(ctx, *v.EntryYear, *v.EntryID) // a posted document names its entry (customer_invoice_posted)
	if err != nil {
		return time.Time{}, fmt.Errorf("receivables: is %s %s cleared: %w", v.Kind, v.DocumentNumber, err)
	}
	if cleared {
		return time.Time{}, problem.Conflict("receivables.document_cleared", "The open item of "+v.DocumentNumber+
			" is cleared (netted when a payment was applied, or cleared against its invoice); undo that clearing first.")
	}
	return date, nil
}

// sourceOf names the kind of document a document entered from another
// context's came from, and how it is corrected there: a debit memo is a
// dunning notice's, whose charge and interest a confirmed dunning run
// posted (T76h) and whose cancellation reverses it (T76i); a credit memo
// is sales'. Neither context enters the other kind.
func sourceOf(v Invoice) string {
	if v.Kind == KindDebitMemo { // its source number is the notice's id
		return "dunning notice " + v.SourceDocumentID.String() + "; it is reversed by cancelling the dunning run that posted it " +
			"(POST /v1/dunning-runs/{id}:cancel), not here."
	}
	return "sales document " + *v.SourceDocumentNumber + // named with its id (customer_invoice_source)
		" (" + v.SourceDocumentID.String() + "); it is corrected there, not reversed here."
}

// markReversed moves the memo from posted to reversed with its reversal.
// ReverseInvoice holds the row's lock and read it as posted; the database
// refuses any other change of a posted document, and a reversal of an
// invoice or of a document entered from another context's
// (customer_invoice_posted_once, customer_invoice_reversed).
func markReversed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, rev ledger.Entry, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE receivables.customer_invoice SET status = 'reversed', reversal_date = $2, reversal_entry_year = $3,
			reversal_entry_id = $4, reversal_reason = $5, version = version + 1, updated_by = $6
		WHERE id = $1`, v.ID, rev.PostingDate, rev.FiscalYear, rev.ID, reason, p.ID); err != nil {
		return fmt.Errorf("receivables: mark %s %s reversed: %w", v.Kind, v.ID, err)
	}
	return nil
}

// recordReversed adds the audit record of the reversal and publishes
// CustomerInvoiceReversed in the same transaction (ADR-0007, ADR-0011).
// Neither carries the reason, which is free text a person wrote (ADR-0079):
// the document keeps it. The event names the customer by id and number,
// never by name (product owner, 2026-11-05), and carries the totals in
// the kind's direction (signed), as CustomerInvoicePosted did.
func (e *Endpoints) recordReversed(ctx context.Context, v Invoice, rev ledger.Entry) error {
	const action = "receivables.customer_invoice.reverse"
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
	if err != nil {
		return fmt.Errorf("receivables: company of %s %s: %w", v.Kind, v.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{v.CustomerID})
	if err != nil {
		return fmt.Errorf("receivables: customer of %s %s: %w", v.Kind, v.ID, err)
	}
	numbers, err := e.Entries.Numbers(ctx, []uuid.UUID{*v.EntryID})
	if err != nil {
		return fmt.Errorf("receivables: journal entry of %s %s: %w", v.Kind, v.ID, err)
	}
	amount := func(m money.Minor) any { return money.EventAmount(signed(v, m), v.Currency, v.Exponent) }
	reversal := map[string]any{"reversal_entry_id": rev.ID, "reversal_entry_year": rev.FiscalYear, "reversal_entry_number": rev.DocumentNumber,
		"reversal_posting_date": rev.PostingDate.Format(time.DateOnly)}
	payload := map[string]any{"customer_invoice_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": names[v.CustomerID].Number, "kind": v.Kind,
		"reference_invoice_id": v.ReferenceInvoiceID, "reference_invoice_number": v.ReferenceInvoiceNumber,
		"net_amount": amount(v.Net), "tax_amount": amount(v.Tax), "gross_amount": amount(v.Gross),
		"journal_entry_id": *v.EntryID, "journal_entry_year": *v.EntryYear, "journal_entry_number": numbers[*v.EntryID],
		"status": v.Status, "status_before": Posted, "version": v.Version}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "kind": v.Kind, "status": v.Status,
		"status_before": Posted, "gross_amount": amount(v.Gross), "journal_entry_id": *v.EntryID}
	for k, x := range reversal {
		payload[k], detail[k] = x, x
	}
	if err := audit.Record(ctx, action, "receivables.customer_invoice", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "customer_invoice_reversed", AggregateType: "receivables.customer_invoice", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of customer invoice %s: %w", action, v.ID, err)
	}
	return nil
}
