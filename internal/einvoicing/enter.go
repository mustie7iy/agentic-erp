package einvoicing

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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

// HeldNotInAED is the hold of a tax invoice not in AED (ADR-0061 §5 C5,
// po:2026-11-09): its VAT is to be kept in AED as it states it (IBT-111,
// reconciled per line with BTAE-08) for the VAT 201, which a supplier
// invoice cannot hold yet, so it is held for a person rather than entered
// with its VAT converted.
const HeldNotInAED = "einvoicing.tax_currency_not_entered"

// Payables enters received e-invoices as supplier invoices and names the
// invoices entered (payables context, ADR-0061 §2, §6), in the caller's
// transaction.
type Payables interface {
	// Enter enters a validated tax invoice. A refusal of the entering is a
	// *problem.Problem below 500, which holds the document; anything else
	// fails the step.
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
// §6): a tax invoice in AED through payables in a savepoint, rolled back
// on any refusal, so neither a constraint the database raised nor the
// entering's partial writes survive and the hold is written in the outer
// transaction, which commits; only infrastructure errors fail the step.
// Credit notes are left received for T65e. Entering a parked invoice
// opens no approval, so nothing in the savepoint registers a step after
// the commit that its rollback would leave behind.
func (e *Endpoints) enter(ctx context.Context, tx pgx.Tx, p reqctx.Principal, operation uuid.UUID, u unprocessed, doc EInvoice, company *Company,
	d Document) error {
	if d.Kind != KindTaxInvoice {
		return nil
	}
	var out entering
	if d.AccountingTax != nil {
		out.held = &Refusal{Code: HeldNotInAED, Detail: "The e-invoice is in " + d.Currency + "; its VAT in AED (IBT-111) is kept as it states it " +
			"for the VAT 201, which a supplier invoice does not hold yet, so enter it by hand."}
	} else {
		var err error
		entry := Entry{LegalEntityID: company.ID, Document: d, SHA256: u.sum, ContentRef: u.ref}
		if out, err = e.tryEnter(ctx, tx, doc.ID, entry); err != nil {
			return err
		}
	}
	return e.entryDecided(ctx, tx, p, operation, doc, company, out)
}

// tryEnter calls payables' entering in a savepoint.
func (e *Endpoints) tryEnter(ctx context.Context, tx pgx.Tx, id uuid.UUID, entry Entry) (entering, error) {
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
	if refused.Status >= 500 {
		return entering{}, err
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT einvoice_entry`); err != nil {
		return entering{}, fmt.Errorf("einvoicing: roll back the refused entering of e-invoice %s: %w", id, err)
	}
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
