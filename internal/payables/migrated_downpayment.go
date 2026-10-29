package payables

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// MigratedDownPayment is a down payment a source system paid before
// migration and had not cleared (SAP special G/L A; T72c, ADR-0037): the
// migration posted it as the supplier's special G/L item, with its input
// tax, in the journal entry named here, as a payment run's release posts a
// request's payment (T46c).
type MigratedDownPayment struct {
	LegalEntityID uuid.UUID
	SupplierID    uuid.UUID
	Currency      string
	Amount        money.Minor // paid, tax included
	TaxCode       *string     // its input tax code, or nil
	Tax           money.Minor // the input tax the amount includes
	PostingDate   time.Time   // when the source system posted it, the request's due date
	Reference     string      // the source system and document: "SAP_PRD 3000/2026/0100000123"
	EntryYear     int16       // the journal entry that posted it
	EntryID       uuid.UUID
}

// TakeOverDownPayment records a migrated down payment as a paid down
// payment request naming the entry that posted it, so it is one as a
// payment run leaves it: listed with the supplier's requests, never paid
// again, and cleared against the final invoice (ClearDownPayments), which
// finds a down payment by its request. The request is numbered from the
// down_payment_request range of its posting date's fiscal year, entered
// open and paid in one step (the lifecycle the database keeps), and
// audited; no event is published, since nothing was requested or paid on
// the platform, as for the quantities of migrated invoices
// (TakeOverInvoiced). The entry's JournalEntryPosted carries the item.
func (e *Endpoints) TakeOverDownPayment(ctx context.Context, in MigratedDownPayment) (DownPaymentRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	if err := e.checkMigratedDownPayment(ctx, in); err != nil {
		return DownPaymentRequest{}, err
	}
	number, err := e.requestNumber(ctx, DownPaymentInput{LegalEntityID: in.LegalEntityID, DueDate: in.PostingDate})
	if err != nil {
		return DownPaymentRequest{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id,
			currency, amount, tax_code, tax_amount, due_date, reference, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		p.TenantID, id, number, in.LegalEntityID, in.SupplierID, in.Currency, in.Amount, in.TaxCode, in.Tax, in.PostingDate, in.Reference, p.ID); err != nil {
		return DownPaymentRequest{}, fmt.Errorf("payables: insert migrated down payment request %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.down_payment_request SET status = $2, journal_entry_year = $3, journal_entry_id = $4, version = version + 1
		WHERE id = $1 AND status = 'open'`, id, RequestPaid, in.EntryYear, in.EntryID); err != nil {
		return DownPaymentRequest{}, fmt.Errorf("payables: mark migrated down payment request %s paid: %w", id, err)
	}
	r, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	return r, audit.Record(ctx, "payables.down_payment_request.take_over", "payables.down_payment_request", r.ID,
		map[string]any{"document_number": r.DocumentNumber, "reference": in.Reference, "amount": r.Amount.Plain(r.Exponent), "currency": r.Currency,
			"journal_entry_id": in.EntryID, "journal_entry_year": in.EntryYear})
}

// checkMigratedDownPayment checks a migrated down payment as a request is
// checked (validateRequest), on the amounts the source system posted: a
// legal entity the caller may act in and a supplier of it, a reference, a
// known currency, an amount above 0, and either no tax or an input tax
// code of the legal entity on the posting date, with one deductible
// component as a down payment's payment posts (T46c), and a tax of 0 or
// more below the amount.
func (e *Endpoints) checkMigratedDownPayment(ctx context.Context, in MigratedDownPayment) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if _, _, err := e.invoiceParties(ctx, Input{LegalEntityID: in.LegalEntityID, SupplierID: in.SupplierID}); err != nil {
		return err
	}
	if _, err := currencyExponent(ctx, tx, in.Currency, "currency"); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(in.Reference) == "" || len(in.Reference) > 100:
		return invalid("payables.invalid_reference", "reference", "The source document's reference has 1 to 100 characters.")
	case in.Amount <= 0:
		return problem.Unprocessable("payables.invalid_amount", "A down payment is more than zero.",
			problem.FieldError{Field: "amount", Code: "not_positive", Message: "an amount above 0"})
	case in.Tax < 0 || in.Tax >= in.Amount || (in.TaxCode == nil && in.Tax != 0):
		return invalid("payables.invalid_tax_amount", "tax", "A down payment's tax is 0 or more and below its amount, and 0 without a tax code.")
	case in.TaxCode == nil:
		return nil
	}
	code, ok, err := e.Accounts.TaxCode(ctx, in.LegalEntityID, *in.TaxCode, in.PostingDate)
	if err != nil {
		return err
	}
	if !ok || code.Direction != "input" {
		return invalid("payables.unknown_tax_code", "tax_code", "Tax code "+*in.TaxCode+" is not an input tax code of the legal entity on the posting date.")
	}
	if _, ok := code.Single(); !ok {
		return componentsUnsupported("tax_code", *in.TaxCode)
	}
	return nil
}
