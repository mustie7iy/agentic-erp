package payables

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// PostInvoice posts a parked invoice: Dr expense per item, Dr input tax on
// the tax code's account (the input tax account of the account
// determination when the items carry none), Cr the supplier's
// reconciliation account. On an order valued through GR/IR (T33) an item
// on an order item clears GR/IR instead of debiting expense: the ledger
// debits GR/IR at the order price of its quantity and puts the rest on
// the price difference account. PO-based invoices need a clean latest
// match or an approved exception.
//
// The kind gives the direction (T34): a credit memo or subsequent credit
// posts the other way round, as journal type KG (Dr supplier, Cr items and
// tax), and a correction naming an invoice posts only while that invoice
// stands, checked under a lock (requireReference).
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
	if err := requireReference(ctx, tx, v); err != nil {
		return Invoice{}, err
	}
	if err := requireCleanMatch(ctx, tx, v); err != nil {
		return Invoice{}, err
	}
	if totalMismatch(v, money.Minor.String) != "" {
		return Invoice{}, problem.Unprocessable("payables.amounts_mismatch", "The gross amount must equal the items plus tax.")
	}
	sup, err := e.postableSupplier(ctx, v)
	if err != nil {
		return Invoice{}, err
	}
	date, err := invoicePostingDate(ctx, tx, v, postingDate)
	if err != nil {
		return Invoice{}, err
	}
	in, terms, err := e.posting(ctx, tx, v, sup, *date)
	if err != nil {
		return Invoice{}, err
	}
	entry, err := e.Postings.Post(ctx, in)
	if err != nil {
		return Invoice{}, err
	}
	if err := markPosted(ctx, tx, p, v, entry, *date, terms); err != nil {
		return Invoice{}, err
	}
	posted, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return posted, recordPosted(ctx, posted, entry)
}

// posting is the entry an invoice posts, with the payment terms it posts
// with (invoiceTerms), which the ledger keeps with its open item.
func (e *Endpoints) posting(ctx context.Context, tx pgx.Tx, v Invoice, sup partner.Supplier, date time.Time) (ledger.Posting, *ledger.ItemTerms, error) {
	order, err := e.orderOf(ctx, v)
	if err != nil {
		return ledger.Posting{}, nil, err
	}
	lines, grir, err := e.invoiceLines(ctx, v, sup, date, grirOnly(order))
	if err != nil {
		return ledger.Posting{}, nil, err
	}
	terms, err := e.invoiceTerms(ctx, tx, v, sup, order)
	if err != nil {
		return ledger.Posting{}, nil, err
	}
	posting := invoicePosting(v, v.ID, date)
	posting.Lines, posting.GRIR, posting.Terms = lines, grir, terms
	return posting, terms, nil
}

// markPosted records an invoice as posted with its entry and the terms it
// posted with.
func markPosted(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, entry ledger.Entry, date time.Time, terms *ledger.ItemTerms) error {
	if err := keepTerms(ctx, tx, p, v, terms); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = 'posted', posting_date = $2, journal_entry_year = $3, journal_entry_id = $4,
		version = version + 1, updated_by = $5 WHERE id = $1`, v.ID, date, entry.FiscalYear, entry.ID, p.ID); err != nil {
		return fmt.Errorf("payables: mark invoice %s posted: %w", v.ID, err)
	}
	return nil
}

// recordPosted audits a posted invoice and publishes SupplierInvoicePosted,
// with its amounts signed by its kind.
func recordPosted(ctx context.Context, posted Invoice, entry ledger.Entry) error {
	return record(ctx, "payables.supplier_invoice.post", posted.ID, map[string]any{"journal_entry_id": entry.ID, "document_number": entry.DocumentNumber},
		event(posted, "supplier_invoice_posted", map[string]any{"document_number": posted.DocumentNumber, "supplier_reference": posted.SupplierReference,
			"invoice_date": posted.InvoiceDate.Format(time.DateOnly), "posting_date": posted.PostingDate.Format(time.DateOnly),
			"gross_amount": money.EventAmount(signed(posted, posted.Gross), posted.Currency, posted.Exponent),
			"tax_amount":   money.EventAmount(signed(posted, posted.Tax), posted.Currency, posted.Exponent), "kind": posted.Kind,
			"purchase_order_id": posted.PurchaseOrderID, "journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "status": Posted,
			"payment_terms": termsEvent(posted.Terms)}))
}

// invoicePosting is the journal entry's header for invoice id of its
// kind, posted on date; the caller adds the lines.
func invoicePosting(v Invoice, id uuid.UUID, date time.Time) ledger.Posting {
	text := map[string]string{KindInvoice: "Supplier invoice ", KindCreditMemo: "Supplier credit memo ", KindSubsequentDebit: "Subsequent debit ",
		KindSubsequentCredit: "Subsequent credit "}[v.Kind] + v.DocumentNumber
	return ledger.Posting{LegalEntityID: v.LegalEntityID, DocumentType: documentType(v.Kind), DocumentDate: v.InvoiceDate,
		PostingDate: date, Currency: v.Currency, Reference: &v.SupplierReference, HeaderText: &text, SourceContext: "payables",
		SourceDocumentID: id}
}

// requireReference lets a correction naming an invoice post only while
// that invoice is posted. The invoice's row is read FOR SHARE, so its
// reversal, which locks it FOR UPDATE and refuses an invoice with posted
// corrections (requireNoCorrections), waits for this posting to commit and
// then sees the correction; a reversal that got there first leaves the
// invoice reversed, which this refuses.
func requireReference(ctx context.Context, tx pgx.Tx, v Invoice) error {
	if v.ReferenceInvoiceID == nil {
		return nil
	}
	var status, number string
	if err := tx.QueryRow(ctx, `SELECT status, document_number FROM payables.supplier_invoice WHERE id = $1 FOR SHARE`, *v.ReferenceInvoiceID).
		Scan(&status, &number); err != nil {
		return fmt.Errorf("payables: lock invoice %s that %s corrects: %w", *v.ReferenceInvoiceID, v.DocumentNumber, err)
	}
	if status != Posted {
		return problem.Conflict("payables.reference_not_posted", fmt.Sprintf("Invoice %s, which %s corrects, is %s; clear the reference or name a posted invoice.",
			number, v.DocumentNumber, status))
	}
	return nil
}

// requireNoCorrections refuses to reverse an invoice that posted
// corrections name: they are reversed first, so none stands against an
// invoice that no longer does (T34).
func requireNoCorrections(ctx context.Context, tx pgx.Tx, v Invoice) error {
	rows, _ := tx.Query(ctx, `SELECT document_number FROM payables.supplier_invoice WHERE reference_invoice_id = $1 AND status = 'posted' ORDER BY 1`, v.ID)
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("payables: read the corrections of invoice %s: %w", v.ID, err)
	}
	if len(numbers) > 0 {
		return problem.Conflict("payables.invoice_has_corrections", fmt.Sprintf("Posted corrections name invoice %s (%s); reverse them first.",
			v.DocumentNumber, strings.Join(numbers, ", ")))
	}
	return nil
}

// documentType is the journal document type a kind posts as: KG for what
// credits the supplier (credit memos, subsequent credits), KR otherwise.
func documentType(kind string) string {
	if credits(kind) {
		return CreditDocumentType
	}
	return DocumentType
}

// postableSupplier is the invoice's supplier when it can be posted to. A
// supplier the partner context no longer finds is refused as unusable with
// that reason, as invoiceParties refuses it, never read as a supplier with
// an empty status.
func (e *Endpoints) postableSupplier(ctx context.Context, v Invoice) (partner.Supplier, error) {
	sup, ok, err := e.Suppliers.Supplier(ctx, v.SupplierID, v.LegalEntityID)
	if err != nil {
		return partner.Supplier{}, err
	}
	why := sup.Usable()
	if !ok {
		why = "no business partner with this id"
	}
	if why != "" {
		return partner.Supplier{}, problem.Unprocessable("payables.supplier_not_usable", "The supplier cannot be posted: "+why+".")
	}
	return sup, nil
}

// requireCleanMatch lets an invoice on a purchase order post only after a
// clean latest match of its current version (F13), or with an approved
// exception request linked that was approved at its current version (F16).
// v is read FOR UPDATE, and a match and an approval lock the invoice too,
// so none lands between this check and the posting. Any change of the
// invoice since the match or the approval (an edit, a hold or release by
// hand) needs a new match or approval, even when the change kept the
// request linked, as the previous release's edit does. A match or approval
// recorded without a version (before migration 000040 or 000041, or by the
// previous release) counts for none.
func requireCleanMatch(ctx context.Context, tx pgx.Tx, v Invoice) error {
	if v.PurchaseOrderID == nil {
		return nil
	}
	var approved, matched *int32
	var clean bool
	// The invoice row always exists (v was just read under lock); the
	// lateral match is NULL when there is none.
	if err := tx.QueryRow(ctx, `SELECT i.exception_version, coalesce(m.is_clean, false), m.invoice_version FROM payables.supplier_invoice i
		LEFT JOIN LATERAL (SELECT is_clean, invoice_version FROM payables.match_result WHERE tenant_id = i.tenant_id AND supplier_invoice_id = i.id
			ORDER BY evaluated_at DESC LIMIT 1) m ON true
		WHERE i.id = $1`, v.ID).Scan(&approved, &clean, &matched); err != nil {
		return fmt.Errorf("payables: read the approval and latest match of invoice %s: %w", v.ID, err)
	}
	switch {
	case approvedAsIs(v, approved):
		return nil
	case clean && matched != nil && *matched == v.Version:
		return nil
	case v.WorkflowRequestID != nil:
		return problem.Conflict("payables.match_required",
			"The approved exception does not cover the invoice as it now stands (it changed since, or was approved by an earlier release); match it again, and have a new exception approved if it has issues.")
	case !clean:
		return problem.Conflict("payables.match_required",
			"An invoice on a purchase order is posted after a clean match or an approved exception; match it first.")
	default:
		return problem.Conflict("payables.match_required",
			"The invoice changed after its last clean match; match it again before it is posted.")
	}
}

// approvedAsIs says whether the invoice has an approved exception request
// linked that was approved at its current version (approved is its
// exception_version): the approval covers the invoice as it stands (F16).
func approvedAsIs(v Invoice, approved *int32) bool {
	return v.WorkflowRequestID != nil && approved != nil && *approved == v.Version
}

// invoicePostingDate is the requested posting date, else the invoice's,
// else today.
func invoicePostingDate(ctx context.Context, tx pgx.Tx, v Invoice, postingDate *time.Time) (*time.Time, error) {
	date := postingDate
	if date == nil {
		date = v.PostingDate
	}
	if date == nil {
		d, err := database.Today(ctx, tx)
		if err != nil {
			return nil, err
		}
		date = &d
	}
	return date, nil
}

// invoiceOrder is the order of an invoice on one valued through GR/IR, the
// order whose items its items clear, or nil: a directly valued order's
// items post their own accounts, as an invoice without an order's do.
func (e *Endpoints) invoiceOrder(ctx context.Context, v Invoice) (*procurement.Order, error) {
	o, err := e.orderOf(ctx, v)
	return grirOnly(o), err
}

// orderOf is the invoice's order, or nil without one; a missing order is
// refused (replayCache.order).
func (e *Endpoints) orderOf(ctx context.Context, v Invoice) (*procurement.Order, error) {
	if v.PurchaseOrderID == nil {
		return nil, nil
	}
	o, err := newReplayCache().order(ctx, e.Orders, v)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// grirOnly is the order if it is valued through GR/IR, else nil.
func grirOnly(o *procurement.Order) *procurement.Order {
	if o == nil || o.Valuation != procurement.ValuationGRIR {
		return nil
	}
	return o
}

// invoiceLines builds the entry: Dr expense per non-zero item, or a GR/IR
// item per item on an order item of an order valued through GR/IR (order
// not nil); Dr input tax on the first item tax code's account; Cr the
// supplier's reconciliation account.
func (e *Endpoints) invoiceLines(ctx context.Context, v Invoice, sup partner.Supplier, date time.Time, order *procurement.Order) ([]ledger.PostingLine,
	[]ledger.GRIRItem, error) {
	var lines []ledger.PostingLine
	var grir []ledger.GRIRItem
	var taxCode *string
	for _, it := range v.Items {
		if it.TaxCode != nil && taxCode == nil {
			taxCode = it.TaxCode
		}
		if order != nil && it.POItemNo != nil {
			item, err := grirItem(*order, v.Kind, it)
			if err != nil {
				return nil, nil, err
			}
			grir = append(grir, item)
			continue
		}
		if it.Amount != 0 {
			// Every item has an account: validation requires one on an item
			// without an order item and fills it from the order item
			// otherwise (orderItem).
			lines = append(lines, ledger.PostingLine{AccountID: *it.AccountID, Amount: signed(v, it.Amount), TaxCode: it.TaxCode, Text: it.Description})
		}
	}
	if v.Tax != 0 {
		account, ok, err := e.taxAccount(ctx, v.LegalEntityID, taxCode, date)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, problem.Unprocessable("payables.no_tax_account", "The legal entity has no input tax account for the invoice's tax.")
		}
		lines = append(lines, ledger.PostingLine{AccountID: account, Amount: signed(v, v.Tax), TaxCode: taxCode})
	}
	lines = append(lines, ledger.PostingLine{AccountID: sup.ReconciliationID, Amount: -signed(v, v.Gross), PartnerID: &v.SupplierID})
	return lines, grir, nil
}

// grirItem is an item's GR/IR item: its quantity at its order item's
// valuation price, the price it was released at (T47a), its amount the
// invoiced net, which the ledger clears GR/IR with, putting the difference
// on price_difference; both in its kind's direction: a credit memo credits
// GR/IR for the quantity it credits, a subsequent debit or credit moves no
// quantity, its amount going to price differences whole (T34). An order
// item the order no longer has is refused, as the match flags it
// (order_item_missing).
func grirItem(o procurement.Order, kind string, it Item) (ledger.GRIRItem, error) {
	quantity, amount := it.Quantity, it.Amount
	if credits(kind) {
		amount = -amount
	}
	if kind == KindCreditMemo {
		quantity = "-" + quantity
	}
	for _, oi := range o.Items {
		if oi.ItemNo == *it.POItemNo {
			return ledger.GRIRItem{PurchaseOrderID: o.ID, POItemNo: oi.ItemNo, Quantity: quantity, Unit: it.Unit, UnitPrice: oi.ValuationPrice,
				PriceUnit: oi.ValuationPriceUnit, Invoiced: &amount, Text: it.Description}, nil
		}
	}
	return ledger.GRIRItem{}, problem.Unprocessable("payables.order_item_missing",
		fmt.Sprintf("Order item %d is not on purchase order %s.", *it.POItemNo, o.DocumentNumber))
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
	// A posted invoice always has its entry: the table's CHECK
	// ((status IN ('posted', 'reversed')) = (journal_entry_id IS NOT NULL))
	// and the pairing of id and year, migration 000007.
	if err := requireNoCorrections(ctx, tx, v); err != nil {
		return Invoice{}, err
	}
	if err := requireNoClearedDownPayments(ctx, tx, v); err != nil {
		return Invoice{}, err
	}
	cleared, err := e.Postings.IsCleared(ctx, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return Invoice{}, err
	}
	if cleared {
		return Invoice{}, problem.Conflict("payables.invoice_cleared", "The invoice's open item is cleared; reverse the payment first.")
	}
	// RunOf takes the proposals lock after this read; the read is only for
	// the clearer refusal: the ledger's reversal checks clearing again under
	// its exclusive lock on the entry.
	run, inRun, err := e.Payments.RunOf(ctx, v.LegalEntityID, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return Invoice{}, err
	}
	if inRun {
		return Invoice{}, problem.Conflict("payables.invoice_in_payment_run", "The invoice is in payment run "+run+". Reject the run once it is proposed; a releasing run pays the invoice (resume a stopped release with :release), then reverse the payment first.")
	}
	if postingDate == nil {
		d, err := database.Today(ctx, tx)
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
		return Invoice{}, fmt.Errorf("payables: mark invoice %s reversed: %w", id, err)
	}
	reversed, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return reversed, record(ctx, "payables.supplier_invoice.reverse", id, map[string]any{"reason": reason, "reversal_entry_id": rev.ID},
		event(reversed, "supplier_invoice_reversed", map[string]any{"document_number": v.DocumentNumber,
			"gross_amount": money.EventAmount(signed(v, v.Gross), v.Currency, v.Exponent), "kind": v.Kind, "journal_entry_id": *v.JournalEntryID,
			"journal_entry_year": *v.JournalEntryYear,
			"reversal_entry_id":  rev.ID, "reversal_entry_year": rev.FiscalYear, "reversal_posting_date": rev.PostingDate.Format(time.DateOnly),
			"status": Reversed}))
}
