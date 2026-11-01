package payables

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
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
	date, err := e.invoicePostingDate(ctx, v, postingDate)
	if err != nil {
		return Invoice{}, err
	}
	if err := e.postable(ctx, v, date); err != nil {
		return Invoice{}, err
	}
	order, err := e.lockedOrder(ctx, tx, v)
	if err != nil {
		return Invoice{}, err
	}
	if totalMismatch(v, money.Minor.String) != "" {
		return Invoice{}, problem.Unprocessable("payables.amounts_mismatch", "The gross amount must equal the items plus tax.")
	}
	sup, err := e.postableSupplier(ctx, v)
	if err != nil {
		return Invoice{}, err
	}
	in, err := e.posting(ctx, tx, v, sup, *date, order)
	if err != nil {
		return Invoice{}, err
	}
	entry, err := e.Postings.Post(ctx, in)
	if err != nil {
		return Invoice{}, err
	}
	if err := e.markPosted(ctx, tx, p, v, sup, entry, *date, in); err != nil {
		return Invoice{}, err
	}
	posted, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return posted, e.recordPosted(ctx, posted, entry)
}

// postable judges an invoice's custom fields and rules before it posts. A
// captured invoice may have landed without a required custom field
// (capturedExtensions); none posts without its required fields. Nor does
// one a rule refuses: a captured invoice held by a rule and released by
// hand is judged again on its header as it stands, with the posting date
// the post uses (the request's, else the invoice's, else the company's
// date), not the one stored.
func (e *Endpoints) postable(ctx context.Context, v Invoice, date *time.Time) error {
	if _, err := e.extensions().Check(ctx, v.Extensions); err != nil {
		return err
	}
	return e.ruleWrites().Check(ctx, storedRuleWrite("payables.supplier_invoice.post", v, Change{PostingDate: &date}))
}

// posting is the entry an invoice on order (nil without one) posts, with
// the payment terms it posts with (invoiceTerms, in Terms) and its supplier's
// withholding taxes (withholding), which the ledger keeps with its open
// item; what is withheld at invoice is credited to the codes' accounts
// instead of the supplier.
func (e *Endpoints) posting(ctx context.Context, tx pgx.Tx, v Invoice, sup partner.Supplier, date time.Time, order *procurement.Order) (ledger.Posting, error) {
	taxDate, err := taxDateOf(ctx, tx, v.TaxDate, v.InvoiceDate, v.ReferenceInvoiceID)
	if err != nil {
		return ledger.Posting{}, err
	}
	t, err := e.calculateTax(ctx, v, taxDate)
	if err != nil {
		return ledger.Posting{}, err
	}
	lines, grir, err := e.invoiceLines(ctx, v, sup, grirOnly(order), t)
	if err != nil {
		return ledger.Posting{}, err
	}
	terms, err := e.invoiceTerms(ctx, tx, v, sup, order)
	if err != nil {
		return ledger.Posting{}, err
	}
	ws, wl, withheld, err := e.withholding(ctx, v, sup)
	if err != nil {
		return ledger.Posting{}, err
	}
	// The supplier line is the last (invoiceLines): it is owed what the
	// invoice does not withhold.
	lines[len(lines)-1].Amount = -signed(v, v.Gross-withheld)
	lines = append(lines, wl...)
	posting := invoicePosting(v, v.ID, date)
	posting.Lines, posting.GRIR, posting.Terms, posting.Withholding = lines, grir, terms, ws
	// A tax date a person set is held to the open returns (P6); the
	// invoice date, or a corrected invoice's, is the document's and is not.
	posting.TaxDate, posting.TaxItems, posting.TaxDateChosen = &taxDate, t.items, v.TaxDate != nil
	return posting, nil
}

// markPosted records an invoice as posted with its entry, the terms it
// posted with and its documentary-requirement warnings at its tax date
// (documentaryWarnings).
func (e *Endpoints) markPosted(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, sup partner.Supplier, entry ledger.Entry, date time.Time,
	in ledger.Posting) error {
	if err := keepTerms(ctx, tx, p, v, in.Terms); err != nil {
		return err
	}
	warnings, err := e.documentaryWarnings(ctx, tx, v, sup, *in.TaxDate)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = 'posted', posting_date = $2, journal_entry_year = $3, journal_entry_id = $4,
		version = version + 1, updated_by = $5, documentary_warnings = $6 WHERE id = $1`, v.ID, date, entry.FiscalYear, entry.ID, p.ID, warnings); err != nil {
		return fmt.Errorf("payables: mark invoice %s posted: %w", v.ID, err)
	}
	return nil
}

// recordPosted audits a posted invoice and publishes SupplierInvoicePosted,
// with its amounts signed by its kind and its custom fields masked.
func (e *Endpoints) recordPosted(ctx context.Context, posted Invoice, entry ledger.Entry) error {
	payload := map[string]any{"document_number": posted.DocumentNumber, "supplier_reference": posted.SupplierReference,
		"invoice_date": posted.InvoiceDate.Format(time.DateOnly), "posting_date": posted.PostingDate.Format(time.DateOnly),
		"gross_amount": money.EventAmount(signed(posted, posted.Gross), posted.Currency, posted.Exponent),
		"tax_amount":   money.EventAmount(signed(posted, posted.Tax), posted.Currency, posted.Exponent), "kind": posted.Kind,
		"purchase_order_id": posted.PurchaseOrderID, "journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "status": Posted,
		"payment_terms": termsEvent(posted.Terms)}
	if err := e.extensions().Event(ctx, payload, posted.Extensions, posted.ExtensionsSchema); err != nil {
		return err
	}
	return record(ctx, "payables.supplier_invoice.post", posted.ID, map[string]any{"journal_entry_id": entry.ID, "document_number": entry.DocumentNumber},
		event(posted, "supplier_invoice_posted", payload))
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

// lockedOrder is the order an invoice posts on, nil without one, read
// under the locks of the order items it names (Orders.LockItems) once the
// invoice may post on its match or approval (requireCleanMatch).
//
// The invoice is matched again there, as :match would match it now, and
// refused (409 payables.match_outdated, left parked) if that finds an
// issue its basis did not show: on a clean match, any; on an approved
// exception, one the approvers did not see (newSinceApproval), as they
// decided on those issues (F16) and not on documents posted since. A match is clean only for the documents posted
// on the order before it: two invoices matched clean before either posts
// would otherwise both post, together invoicing more than was received
// (two credit memos crediting more than was invoiced, a subsequent debit
// matched before its invoice was reversed), and an order change or a
// change of the tolerances since may put an item outside them. The locks
// make another posting on the items, and a goods movement on them, wait
// for this one, so what the check read of them holds until it commits;
// reversals of invoices and credit memos take the same locks and are
// checked against the same quantities (reversible, 03 §3.9).
func (e *Endpoints) lockedOrder(ctx context.Context, tx pgx.Tx, v Invoice) (*procurement.Order, error) {
	approved, err := requireCleanMatch(ctx, tx, v)
	if err != nil || v.PurchaseOrderID == nil {
		return nil, err
	}
	if nos := orderItemNos(v); len(nos) > 0 {
		if err := e.Orders.LockItems(ctx, *v.PurchaseOrderID, nos); err != nil {
			return nil, err
		}
	}
	j, err := e.judge(ctx, tx, v)
	if err != nil {
		return nil, err
	}
	issues := j.issues
	if approved && len(issues) > 0 {
		if issues, err = newSinceApproval(ctx, tx, v, issues); err != nil {
			return nil, err
		}
	}
	if len(issues) > 0 {
		return nil, matchOutdated(issues)
	}
	return &j.r.Order, nil
}

// newSinceApproval are the issues an invoice on its approved exception
// has now that the approvers did not see: those not in the latest match
// before the approval moved the invoice to the version it was approved at
// (exception_version, v.Version here), which is the match the request was
// opened on or a later one while it was on hold. Issues are compared with
// their messages, which carry the figures (the quantities invoiced and
// received, the amounts and the tolerance): one that has grown since (more
// invoiced past the same receipt) is new, and so is one whose figures
// moved otherwise (more received since), which a match then holds for the
// approvers to decide again (applyMatch).
func newSinceApproval(ctx context.Context, tx pgx.Tx, v Invoice, issues []Issue) ([]Issue, error) {
	var seen []Issue
	err := tx.QueryRow(ctx, `SELECT issues FROM payables.match_result WHERE supplier_invoice_id = $1 AND invoice_version < $2
		ORDER BY evaluated_at DESC LIMIT 1`, v.ID, v.Version).Scan(&seen)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("payables: read the match invoice %s was approved on: %w", v.ID, err)
	}
	key := func(is Issue) string { return is.Code + "\x00" + is.Message }
	approved := map[string]bool{}
	for _, is := range seen {
		approved[key(is)] = true
	}
	var out []Issue
	for _, is := range issues {
		if !approved[key(is)] {
			out = append(out, is)
		}
	}
	return out, nil
}

// orderItemNos are the order items the invoice's items name, ascending
// and once each, the order Orders.LockItems locks them in.
func orderItemNos(v Invoice) []int32 {
	var nos []int32
	for _, it := range v.Items {
		if it.POItemNo != nil {
			nos = append(nos, *it.POItemNo)
		}
	}
	slices.Sort(nos)
	return slices.Compact(nos)
}

// matchOutdated refuses to post an invoice on a clean match that a match
// now would not find clean, with the issues it would find.
func matchOutdated(issues []Issue) error {
	why := make([]string, len(issues))
	for i, is := range issues {
		why[i] = is.Message
	}
	return problem.Conflict("payables.match_outdated", "The invoice no longer matches its order as it did when it was matched: "+strings.Join(why, "; ")+
		". Documents posted on the order since its match or approval, a change of the order, or of the tolerances, leave it with issues; match it again.")
}

// requireCleanMatch lets an invoice on a purchase order post only after a
// clean latest match of its current version (F13), or with an approved
// exception request linked that was approved at its current version (F16);
// it returns true when the approval is what lets it post.
// v is read FOR UPDATE, and a match and an approval lock the invoice too,
// so none lands between this check and the posting. Any change of the
// invoice since the match or the approval (an edit, a hold or release by
// hand) needs a new match or approval, even when the change kept the
// request linked, as the previous release's edit does. A match or approval
// recorded without a version (before migration 000040 or 000041, or by the
// previous release) counts for none.
func requireCleanMatch(ctx context.Context, tx pgx.Tx, v Invoice) (bool, error) {
	if v.PurchaseOrderID == nil {
		return false, nil
	}
	var exception, matched *int32
	var clean bool
	// The invoice row always exists (v was just read under lock); the
	// lateral match is NULL when there is none.
	if err := tx.QueryRow(ctx, `SELECT i.exception_version, coalesce(m.is_clean, false), m.invoice_version FROM payables.supplier_invoice i
		LEFT JOIN LATERAL (SELECT is_clean, invoice_version FROM payables.match_result WHERE tenant_id = i.tenant_id AND supplier_invoice_id = i.id
			ORDER BY evaluated_at DESC LIMIT 1) m ON true
		WHERE i.id = $1`, v.ID).Scan(&exception, &clean, &matched); err != nil {
		return false, fmt.Errorf("payables: read the approval and latest match of invoice %s: %w", v.ID, err)
	}
	switch {
	case approvedAsIs(v, exception):
		return true, nil
	case clean && matched != nil && *matched == v.Version:
		return false, nil
	case v.WorkflowRequestID != nil:
		return false, problem.Conflict("payables.match_required",
			"The approved exception does not cover the invoice as it now stands (it changed since, or was approved by an earlier release); match it again, and have a new exception approved if it has issues.")
	case !clean:
		return false, problem.Conflict("payables.match_required",
			"An invoice on a purchase order is posted after a clean match or an approved exception; match it first.")
	default:
		return false, problem.Conflict("payables.match_required",
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
// else the company's date now in its time zone (companyDay).
func (e *Endpoints) invoicePostingDate(ctx context.Context, v Invoice, postingDate *time.Time) (*time.Time, error) {
	date := postingDate
	if date == nil {
		date = v.PostingDate
	}
	if date == nil {
		d, err := e.companyDay(ctx, v.LegalEntityID)
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

// invoiceLines builds the entry (T61d): Dr expense per non-zero item, or a
// GR/IR item per item on an order item of an order valued through GR/IR
// (order not nil), each with the tax it bears (a non-deductible
// component's, a part not recovered); the tax lines, one per code and
// component with an account (calculateTax), or, without coded items, the stated
// tax on the input tax account; Cr the supplier's reconciliation account.
func (e *Endpoints) invoiceLines(ctx context.Context, v Invoice, sup partner.Supplier, order *procurement.Order, t invoiceTax) ([]ledger.PostingLine,
	[]ledger.GRIRItem, error) {
	var lines []ledger.PostingLine
	var grir []ledger.GRIRItem
	for i, it := range v.Items {
		amount := it.Amount + t.borne[i] // the item and tax on it: in range as the gross is
		if order != nil && it.POItemNo != nil {
			item, err := grirItem(*order, v.Kind, it, amount)
			if err != nil {
				return nil, nil, err
			}
			grir = append(grir, item)
			continue
		}
		if amount != 0 {
			// Every item has an account: validation requires one on an item
			// without an order item and fills it from the order item
			// otherwise (orderItem).
			lines = append(lines, ledger.PostingLine{AccountID: *it.AccountID, Amount: signed(v, amount), TaxCode: it.TaxCode, Text: it.Description,
				CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID})
		}
	}
	if t.items == nil && v.Tax != 0 {
		account, ok, err := e.Accounts.Determination(ctx, v.LegalEntityID, "input_tax")
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, problem.Unprocessable("payables.no_tax_account", "The legal entity has no input tax account for the invoice's tax.")
		}
		lines = append(lines, ledger.PostingLine{AccountID: account, Amount: signed(v, v.Tax)})
	}
	lines = append(lines, t.lines...)
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
func grirItem(o procurement.Order, kind string, it Item, amount money.Minor) (ledger.GRIRItem, error) {
	quantity := it.Quantity
	if credits(kind) {
		amount = -amount
	}
	if kind == KindCreditMemo {
		quantity = "-" + quantity
	}
	for _, oi := range o.Items {
		if oi.ItemNo == *it.POItemNo {
			return ledger.GRIRItem{PurchaseOrderID: o.ID, POItemNo: oi.ItemNo, Quantity: quantity, Unit: it.Unit, UnitPrice: oi.ValuationPrice,
				PriceUnit: oi.ValuationPriceUnit, Invoiced: &amount, TaxCode: it.TaxCode, Text: it.Description, CostCenterID: oi.CostCenterID}, nil
		}
	}
	return ledger.GRIRItem{}, problem.Unprocessable("payables.order_item_missing",
		fmt.Sprintf("Order item %d is not on purchase order %s.", *it.POItemNo, o.DocumentNumber))
}

// requireUnpaid refuses a reversal of an invoice whose open item is cleared
// or in a payment run.
func (e *Endpoints) requireUnpaid(ctx context.Context, v Invoice) error {
	cleared, err := e.Postings.IsCleared(ctx, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return err
	}
	if cleared {
		return problem.Conflict("payables.invoice_cleared", "The invoice's open item is cleared; reverse the payment first.")
	}
	// RunOf takes the proposals lock after this read; the read is only for
	// the clearer refusal: the ledger's reversal checks clearing again under
	// its exclusive lock on the entry.
	run, inRun, err := e.Payments.RunOf(ctx, v.LegalEntityID, *v.JournalEntryYear, *v.JournalEntryID)
	if err != nil {
		return err
	}
	if inRun {
		return problem.Conflict("payables.invoice_in_payment_run", "The invoice is in payment run "+run+". Reject the run once it is proposed; a releasing run pays the invoice (resume a stopped release with :release), then reverse the payment first.")
	}
	return nil
}

// ReverseInvoice reverses a posted invoice's entry; an invoice whose open
// item is cleared (paid) or in a live payment run cannot be reversed, nor
// one whose reversal would leave its order items past what the match
// holds postings to (reversible).
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
	if err := e.requireUnpaid(ctx, v); err != nil {
		return Invoice{}, err
	}
	if err := e.reversible(ctx, tx, v); err != nil {
		return Invoice{}, err
	}
	if postingDate == nil {
		d, err := e.companyDay(ctx, v.LegalEntityID)
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
