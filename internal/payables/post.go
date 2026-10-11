package payables

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// PostInvoice posts a parked invoice: Dr expense per item, Dr input tax on
// the tax code's account (the input tax account of the account
// determination when the items carry none), Cr the supplier's
// reconciliation account. PO-based invoices need a clean latest match or
// an approved exception.
func (e *Endpoints) PostInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (Invoice, error) {
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
	if err := requireStatus(v, "post", Parked); err != nil {
		return Invoice{}, err
	}
	if v.PurchaseOrderID != nil && v.WorkflowRequestID == nil {
		var clean *bool
		err := tx.QueryRow(ctx, `SELECT is_clean FROM payables.match_result WHERE supplier_invoice_id = $1 ORDER BY evaluated_at DESC LIMIT 1`, id).Scan(&clean)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, err
		}
		if clean == nil || !*clean {
			return Invoice{}, problem.Conflict("payables.match_required",
				"An invoice on a purchase order is posted after a clean match or an approved exception; match it first.")
		}
	}
	var net int64
	for _, it := range v.Items {
		net += it.Amount
	}
	if net+v.Tax != v.Gross {
		return Invoice{}, problem.Unprocessable("payables.amounts_mismatch", "The gross amount must equal the items plus tax.")
	}
	sup, _, err := e.Suppliers.Supplier(ctx, v.SupplierID, v.LegalEntityID)
	if err != nil {
		return Invoice{}, err
	}
	if why := sup.Usable(); why != "" {
		return Invoice{}, problem.Unprocessable("payables.supplier_not_usable", "The supplier cannot be posted: "+why+".")
	}
	date := postingDate
	if date == nil {
		date = v.PostingDate
	}
	if date == nil {
		d, err := today(ctx, tx)
		if err != nil {
			return Invoice{}, err
		}
		date = &d
	}
	var lines []ledger.PostingLine
	var taxCode *string
	for _, it := range v.Items {
		if it.TaxCode != nil && taxCode == nil {
			taxCode = it.TaxCode
		}
		if it.Amount != 0 {
			lines = append(lines, ledger.PostingLine{AccountID: *it.AccountID, Amount: it.Amount, TaxCode: it.TaxCode, Text: it.Description})
		}
	}
	if v.Tax != 0 {
		account, ok, err := e.taxAccount(ctx, v.LegalEntityID, taxCode, *date)
		if err != nil {
			return Invoice{}, err
		}
		if !ok {
			return Invoice{}, problem.Unprocessable("payables.no_tax_account", "The legal entity has no input tax account for the invoice's tax.")
		}
		lines = append(lines, ledger.PostingLine{AccountID: account, Amount: v.Tax, TaxCode: taxCode})
	}
	lines = append(lines, ledger.PostingLine{AccountID: sup.ReconciliationID, Amount: -v.Gross, PartnerID: &v.SupplierID})
	text := "Supplier invoice " + v.DocumentNumber
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: v.LegalEntityID, DocumentType: DocumentType, DocumentDate: v.InvoiceDate,
		PostingDate: *date, Currency: v.Currency, Reference: &v.SupplierReference, HeaderText: &text, SourceContext: "payables",
		SourceDocumentID: id, Lines: lines})
	if err != nil {
		return Invoice{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = 'posted', posting_date = $2, journal_entry_year = $3, journal_entry_id = $4,
		version = version + 1, updated_by = $5 WHERE id = $1`, id, *date, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return Invoice{}, err
	}
	posted, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return posted, record(ctx, "payables.supplier_invoice.post", id, map[string]any{"journal_entry_id": entry.ID, "document_number": entry.DocumentNumber},
		event(posted, "supplier_invoice_posted", map[string]any{"document_number": posted.DocumentNumber, "supplier_reference": posted.SupplierReference,
			"invoice_date": posted.InvoiceDate.Format(time.DateOnly), "posting_date": date.Format(time.DateOnly),
			"gross_amount": amount(posted.Gross, posted.Currency, posted.Exponent), "tax_amount": amount(posted.Tax, posted.Currency, posted.Exponent),
			"purchase_order_id": posted.PurchaseOrderID, "journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "status": Posted}))
}

// taxAccount is the account of the tax code valid on the date, else the
// legal entity's input tax account.
func (e *Endpoints) taxAccount(ctx context.Context, le uuid.UUID, code *string, date time.Time) (uuid.UUID, bool, error) {
	if code != nil {
		t, ok, err := e.Accounts.TaxCode(ctx, le, *code, date)
		if err != nil || ok {
			return t.AccountID, ok, err
		}
	}
	return e.Accounts.Determination(ctx, le, "input_tax")
}

// ReverseInvoice reverses a posted invoice's entry; an invoice whose open
// item is cleared (paid) or in a live payment run cannot be reversed.
func (e *Endpoints) ReverseInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string, postingDate *time.Time) (Invoice, error) {
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
	if err := requireStatus(v, "reverse", Posted); err != nil {
		return Invoice{}, err
	}
	cleared, err := e.Postings.IsCleared(ctx, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return Invoice{}, err
	}
	if cleared {
		return Invoice{}, problem.Conflict("payables.invoice_cleared", "The invoice's open item is cleared; reverse the payment first.")
	}
	run, inRun, err := e.Payments.RunOf(ctx, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return Invoice{}, err
	}
	if inRun {
		return Invoice{}, problem.Conflict("payables.invoice_in_payment_run", "The invoice is in payment run "+run+"; reject the run first.")
	}
	if postingDate == nil {
		d, err := today(ctx, tx)
		if err != nil {
			return Invoice{}, err
		}
		postingDate = &d
	}
	text := "Reversal of supplier invoice " + v.DocumentNumber
	rev, err := e.Postings.Reverse(ctx, *v.JournalEntryID, postingDate, &text)
	if err != nil {
		return Invoice{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = 'reversed', reversal_entry_year = $2, reversal_entry_id = $3,
		version = version + 1, updated_by = $4 WHERE id = $1`, id, rev.FiscalYear, rev.ID, p.ID); err != nil {
		return Invoice{}, err
	}
	reversed, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return reversed, record(ctx, "payables.supplier_invoice.reverse", id, map[string]any{"reason": reason, "reversal_entry_id": rev.ID},
		event(reversed, "supplier_invoice_reversed", map[string]any{"document_number": v.DocumentNumber,
			"gross_amount": amount(v.Gross, v.Currency, v.Exponent), "journal_entry_id": *v.JournalEntryID, "journal_entry_year": *v.JournalEntryYear,
			"reversal_entry_id": rev.ID, "reversal_entry_year": rev.FiscalYear, "reversal_posting_date": rev.PostingDate.Format(time.DateOnly),
			"status": Reversed}))
}
