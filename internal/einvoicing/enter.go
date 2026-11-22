package einvoicing

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses an e-invoice's entering moves it to (ADR-0061 §6, §7; T65d):
// entered as a supplier invoice, or held with the problem of its entering
// for a person (T65f).
const (
	StatusEntered = "entered"
	StatusHeld    = "held"
)

// HeldNotInAED is the hold of a document not in AED that does not state
// each line's VAT in AED (BTAE-08; ADR-0061 §5 C5, po:2026-11-09,
// po:2026-11-21): its VAT is kept in AED as it states it for the VAT 201,
// so without it the document is held for a person rather than entered
// with its VAT converted. One stating it is entered keeping it (As built
// batch 88).
const HeldNotInAED = "einvoicing.tax_currency_not_entered"

// HeldNoInvoice is the hold of a tax credit note naming no invoice (ADR-0061
// §5 C4, po:2026-11-09; T65e): a UAE tax credit note names the invoice it
// corrects (IBG-03, IBT-025) unless it is for a volume discount (BTAE-03
// VD, IBR-055-AE; po:2026-11-21), so any other naming none goes to a
// person rather than being entered as a credit memo correcting none, as
// capture would enter a paper one. A credit note citing several invoices
// is entered as one credit memo citing them all (po:2026-11-21); the hold
// einvoicing.credit_note_names_several_invoices of T65e is no longer
// given, and documents held with it before stay held.
const HeldNoInvoice = "einvoicing.credit_note_names_no_invoice"

// Payables enters received e-invoices as supplier invoices and names the
// invoices entered (payables context, ADR-0061 §2, §6), in the caller's
// transaction.
type Payables interface {
	// Enter enters a validated tax invoice as an invoice, or a tax credit
	// note as a credit memo naming the invoice it cites (IBT-025). A
	// refusal of the entering is a *problem.Problem below 500, which holds
	// the document; anything else fails the step.
	Enter(ctx context.Context, e Entry) (Entered, error)
	// Invoices names entered invoices by id: their number, the supplier
	// reference, status and supplier.
	Invoices(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]EnteredInvoice, error)
}

// Entry is a validated document to enter: the company it is addressed
// to, what was read, and its bytes' hash and key, which become the
// invoice's document.
type Entry struct {
	LegalEntityID uuid.UUID
	Document      Document
	SHA256        []byte
	ContentRef    string
}

// Entered is the supplier invoice a document was entered as.
type Entered struct {
	InvoiceID, SupplierID uuid.UUID
}

// EnteredInvoice is an entered invoice as the list names it.
type EnteredInvoice struct {
	DocumentNumber, SupplierReference, Status string
	SupplierID                                uuid.UUID
	SupplierName                              string
}

// entering is the outcome of entering one document: the invoice, or the
// code and detail of the problem that holds it.
type entering struct {
	entered *Entered
	held    *Refusal
}

// enter enters the locked, valid document as a supplier invoice (ADR-0061
// §6): a tax invoice or tax credit note in AED through payables in a
// savepoint, rolled back on any refusal, so neither a constraint the
// database raised nor the entering's partial writes survive and the hold
// is written in the outer transaction, which commits; only infrastructure
// errors fail the step. A credit note is entered as a credit memo naming
// the invoice it cites as payables matches it (T65e: a posted invoice of the
// supplier, or on hold naming the number), or one citing several as one
// credit memo citing them all; one citing none is held unless it is for a
// volume discount (heldBefore). Entering a parked invoice or credit memo opens no
// approval, so nothing in the savepoint registers a step after the commit
// that its rollback would leave behind.
func (e *Endpoints) enter(ctx context.Context, tx pgx.Tx, p reqctx.Principal, operation uuid.UUID, u unprocessed, doc EInvoice, company *Company,
	d Document) error {
	out := entering{held: heldBefore(d)}
	if out.held == nil {
		var err error
		entry := Entry{LegalEntityID: company.ID, Document: d, SHA256: u.sum, ContentRef: u.ref}
		if out, err = e.tryEnter(ctx, tx, doc.ID, entry); err != nil {
			return err
		}
	}
	return e.entryDecided(ctx, tx, p, operation, doc, company, out)
}

// heldBefore is the hold of a valid document that is not handed to
// payables at all: one not in AED without each line's VAT in AED (C5),
// and a credit note naming no invoice that is not for a volume discount
// (C4, IBR-055-AE); nil for one to enter.
func heldBefore(d Document) *Refusal {
	_, stated, _ := LineTaxInAED(d) // out of range is not stated, and validation refused such a document (inconsistentInAED)
	switch {
	case d.Currency != accountingCurrency && !stated:
		return &Refusal{Code: HeldNotInAED, Detail: "The e-invoice is in " + d.Currency + " and does not state each line's VAT in AED (BTAE-08), " +
			"which is kept as it states it for the VAT 201, so enter it by hand."}
	case d.Kind == KindTaxCreditNote && len(d.Cited) == 0 && !d.volumeDiscount():
		return &Refusal{Code: HeldNoInvoice, Detail: "The tax credit note names no invoice it corrects (IBG-03, IBT-025) and is not for a " +
			"volume discount (BTAE-03 VD); enter it by hand naming the invoice, after asking the supplier which one it corrects."}
	}
	return nil
}

// tryEnter calls payables' entering in a savepoint. The audit entries the
// entering buffered (a rule's hold among them) are dropped with its
// rollback, so no record names an invoice that never existed. A problem
// that asks to be retried (a lock timeout, a serialization failure: one
// with a Retry-After) is the machinery, not a refusal: it fails the step,
// the document staying received for the next try.
func (e *Endpoints) tryEnter(ctx context.Context, tx pgx.Tx, id uuid.UUID, entry Entry) (entering, error) {
	mark := audit.Mark(ctx)
	if _, err := tx.Exec(ctx, `SAVEPOINT einvoice_entry`); err != nil {
		return entering{}, fmt.Errorf("einvoicing: open the savepoint to enter e-invoice %s: %w", id, err)
	}
	// The fetch's system principal acts for the company the document is
	// addressed to, and for no other, while it enters it (ADR-0061 §4).
	entered, err := e.Payables.Enter(authz.ActingFor(ctx, entry.LegalEntityID), entry)
	if err == nil {
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT einvoice_entry`); err != nil {
			return entering{}, fmt.Errorf("einvoicing: release the savepoint of e-invoice %s: %w", id, err)
		}
		return entering{entered: &entered}, nil
	}
	refused := problem.From(err)
	if refused.Status >= 500 || refused.RetryAfter > 0 {
		return entering{}, err
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT einvoice_entry`); err != nil {
		return entering{}, fmt.Errorf("einvoicing: roll back the refused entering of e-invoice %s: %w", id, err)
	}
	audit.DiscardFrom(ctx, mark)
	return entering{held: &Refusal{Code: refused.Code, Detail: refused.Detail}}, nil
}

// entryDecided writes the outcome of the entering on the locked document:
// entered, naming the invoice and its supplier, or held with the
// problem's code and detail; with the audit record and EInvoiceChanged.
// The update is guarded on the document still received and processed.
func (e *Endpoints) entryDecided(ctx context.Context, tx pgx.Tx, p reqctx.Principal, operation uuid.UUID, doc EInvoice, company *Company,
	out entering) error {
	status, code, detail, invoice, supplier := StatusHeld, (*string)(nil), (*string)(nil), (*uuid.UUID)(nil), (*uuid.UUID)(nil)
	if out.entered != nil {
		status, invoice, supplier = StatusEntered, &out.entered.InvoiceID, &out.entered.SupplierID
	} else {
		d := clipDetail(out.held.Detail)
		code, detail = &out.held.Code, &d
	}
	after, err := scanEInvoice(tx.QueryRow(ctx, `UPDATE einvoicing.einvoice SET status = $2, problem_code = $3, problem_detail = $4, entry_at = now(),
			entry_fetch_operation_id = $5, supplier_invoice_id = $6, supplier_id = $7, version = version + 1, updated_by = $8
		WHERE id = $1 AND status = 'received' AND processed_at IS NOT NULL RETURNING `+einvoiceColumns,
		doc.ID, status, code, detail, operation, invoice, supplier, p.ID))
	if err != nil {
		return fmt.Errorf("einvoicing: record the entering of e-invoice %s: %w", doc.ID, err)
	}
	if out.entered != nil {
		return recordChange(ctx, after, company, entered)
	}
	return recordChange(ctx, after, company, held)
}

// clipDetail keeps a problem's detail within the column's 2,000
// characters.
func clipDetail(s string) string {
	if r := []rune(s); len(r) > 2000 {
		return string(r[:1999]) + "…"
	}
	return s
}
