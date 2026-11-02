package payables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ClearingDocumentType is the journal document type of the transfer that
// clears down payments against an invoice (SAP AB; journal_ab).
const ClearingDocumentType = "AB"

// ClearDownPayments clears paid down payment requests of the invoice's
// supplier against the posted invoice (SAP F-54; T46d, ADR-0037), whole:
// the transfer (ledger.Postings.ClearDownPayments) clears the down
// payments, reverses their input tax and debits the supplier with their
// total under the invoice's payment terms, so the next run pays the
// invoice less them and takes any cash discount on what remains. The
// requests are cleared, audited, and DownPaymentsCleared is published.
func (e *Endpoints) ClearDownPayments(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate time.Time, items []ledger.ItemRef) (Invoice, error) {
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
	if err := requireStatus(v, "clearing down payments", Posted); err != nil {
		return Invoice{}, err
	}
	if v.Kind != KindInvoice {
		return Invoice{}, invalid("payables.not_an_invoice", "id", "Down payments are cleared against an invoice, not a "+v.Kind+".")
	}
	if err := ledger.RequireDistinct(items); err != nil {
		return Invoice{}, err
	}
	// A run holding the invoice pays its gross as proposed, which would
	// leave the transfer's debit unpaid back: the run is rejected first.
	// Asking holds off proposals until this commits, so none picks the
	// invoice up before the transfer's debit is there to net with it.
	run, inRun, err := e.Payments.RunOf(ctx, v.LegalEntityID, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return Invoice{}, err
	}
	if inRun {
		return Invoice{}, problem.Conflict("payables.invoice_in_payment_run", "The invoice is in payment run "+run+"; reject the run before clearing down payments against it.")
	}
	requests, total, err := clearedRequests(ctx, tx, v, items)
	if err != nil {
		return Invoice{}, err
	}
	sup, err := e.postableSupplier(ctx, v)
	if err != nil {
		return Invoice{}, err
	}
	text := "Down payments cleared against " + v.DocumentNumber
	entry, err := e.Postings.ClearDownPayments(ctx, ledger.DownPaymentClearing{LegalEntityID: v.LegalEntityID, SupplierID: v.SupplierID,
		APAccountID: sup.ReconciliationID, DocumentType: ClearingDocumentType, PostingDate: postingDate, Currency: v.Currency, Items: items,
		Terms: v.Terms, Reference: &v.SupplierReference, HeaderText: &text, SourceContext: "payables", SourceDocumentID: v.ID})
	if err != nil {
		return Invoice{}, err
	}
	if err := requireWholeRequests(entry, sup.ReconciliationID, total); err != nil {
		return Invoice{}, err
	}
	return v, markCleared(ctx, tx, p, v, requests, total, entry)
}

// requireWholeRequests refuses the clearing (422
// payables.not_a_down_payment_request) when the transfer's payables debit,
// what the ledger cleared, is not the total of the requests found: the
// items are then not exactly the down payments of those requests (one of
// a request's lines left out, or an entry paying several requests), and
// marking the requests cleared would record an amount the ledger did not
// clear. A run posts one entry per request with one down payment, and the
// migration refuses more than one open down payment in a document
// (ADR-0037, As built T72c), so no path reaches this today; it holds the
// money if one ever does (review:batch39). The refusal rolls the transfer
// back with the unit of work.
func requireWholeRequests(entry ledger.Entry, apAccount uuid.UUID, total money.Minor) error {
	debits := 0
	var debit money.Minor
	for _, l := range entry.Lines {
		if l.AccountID == apAccount && l.SpecialGL == nil {
			debits, debit = debits+1, l.TC
		}
	}
	if debits != 1 || debit != total {
		return invalid("payables.not_a_down_payment_request", "items",
			"The items are not the whole down payments of the paid requests they belong to: choose every down payment of a request's payment, and no other.")
	}
	return nil
}

// clearedRequests are the paid requests whose down payments the items are,
// locked, with their total: each item is a request's down payment (its
// payment entry) of the invoice's supplier, legal entity and currency, on
// the invoice's purchase order when it names one, and they, with those
// cleared against the invoice before, are at most its gross amount, since
// only whole down payments are cleared. A request counts once however
// many of the items its payment entry holds (review:batch39); an entry
// naming several requests, which ADR-0037 rules out, is resolved to the
// same one every time, so requireWholeRequests refuses it.
func clearedRequests(ctx context.Context, tx pgx.Tx, v Invoice, items []ledger.ItemRef) ([]DownPaymentRequest, money.Minor, error) {
	amounts := []money.Minor{}
	var requests []DownPaymentRequest
	counted := map[uuid.UUID]bool{}
	for i, it := range items {
		r, err := scanRequest(tx.QueryRow(ctx, requestSelect+` WHERE r.journal_entry_year = $1 AND r.journal_entry_id = $2 AND r.status IN ('paid', 'cleared')
			AND r.supplier_id = $3 AND r.legal_entity_id = $4 AND r.currency = $5 ORDER BY r.id LIMIT 1 FOR UPDATE OF r`, it.FiscalYear, it.EntryID, v.SupplierID, v.LegalEntityID, v.Currency))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, invalid("payables.not_a_down_payment_request", fmt.Sprintf("items[%d]", i),
				"The item is not the down payment of a paid request of the invoice's supplier in its currency.")
		}
		if err != nil {
			return nil, 0, fmt.Errorf("payables: the down payment request of item %d of invoice %s: %w", i+1, v.ID, err)
		}
		if r.Status == RequestCleared {
			return nil, 0, problem.Conflict("payables.down_payment_already_cleared", "Down payment request "+r.DocumentNumber+" is cleared already.")
		}
		if r.PurchaseOrderID != nil && (v.PurchaseOrderID == nil || *v.PurchaseOrderID != *r.PurchaseOrderID) {
			return nil, 0, invalid("payables.down_payment_other_order", fmt.Sprintf("items[%d]", i),
				"Down payment request "+r.DocumentNumber+" is for another purchase order than the invoice's.")
		}
		if counted[r.ID] {
			continue
		}
		counted[r.ID] = true
		requests, amounts = append(requests, r), append(amounts, r.Amount)
	}
	var before money.Minor
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount), 0) FROM payables.down_payment_request WHERE cleared_invoice_id = $1`, v.ID).Scan(&before); err != nil {
		return nil, 0, fmt.Errorf("payables: the down payments cleared against invoice %s: %w", v.ID, err)
	}
	total, err := money.Sum(amounts...)
	var all money.Minor
	if err == nil {
		all, err = money.Sum(total, before)
	}
	if err != nil || all > v.Gross {
		return nil, 0, invalid("payables.down_payments_exceed_invoice", "items",
			"Only whole down payments are cleared, and with those cleared before they exceed the invoice's gross amount.")
	}
	return requests, total, nil
}

// requireNoClearedDownPayments refuses to reverse an invoice that down
// payments were cleared against (409 payables.down_payments_cleared): its
// transfer's payables debit stands on the invoice.
func requireNoClearedDownPayments(ctx context.Context, tx pgx.Tx, v Invoice) error {
	var cleared bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM payables.down_payment_request WHERE cleared_invoice_id = $1)`, v.ID).Scan(&cleared); err != nil {
		return fmt.Errorf("payables: the down payments cleared against invoice %s: %w", v.ID, err)
	}
	if cleared {
		return problem.Conflict("payables.down_payments_cleared", "Down payments were cleared against the invoice; it is not reversed while they stand.")
	}
	return nil
}

// markCleared moves the requests to cleared against the invoice by the
// transfer entry, audits each and the invoice, and publishes
// DownPaymentsCleared.
func markCleared(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, requests []DownPaymentRequest, total money.Minor, entry ledger.Entry) error {
	ids := make([]uuid.UUID, len(requests))
	for i, r := range requests {
		ids[i] = r.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.down_payment_request SET status = 'cleared', cleared_invoice_id = $2, clearing_entry_year = $3,
		clearing_entry_id = $4, version = version + 1, updated_by = $5 WHERE id = ANY($1) AND status = 'paid'`,
		ids, v.ID, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return fmt.Errorf("payables: clear the down payments of invoice %s: %w", v.ID, err)
	}
	audits := make([]error, 0, len(requests)+1)
	for _, r := range requests {
		audits = append(audits, recordRequest(ctx, "payables.down_payment_request.clear", r, map[string]any{"supplier_invoice_id": v.ID,
			"clearing_entry_id": entry.ID}))
	}
	return errors.Join(append(audits, record(ctx, "payables.supplier_invoice.clear_down_payments", v.ID, map[string]any{"clearing_entry_id": entry.ID,
		"down_payment_request_ids": ids}, event(v, "down_payments_cleared", map[string]any{"clearing_entry_id": entry.ID,
		"clearing_entry_year": entry.FiscalYear, "down_payment_request_ids": ids, "amount": money.EventAmount(total, v.Currency, v.Exponent)})))...)
}
