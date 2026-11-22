package payables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreditReason is a credit memo's reason as its e-invoice states it
// (BTAE-03; ADR-0061 As built batch 88): its code and its words.
type CreditReason struct{ Code, Text string }

// Citation is a preceding invoice a credit memo's e-invoice cites (IBG-03):
// its number (IBT-025), its issue date when stated (IBT-026), and the
// posted invoice of the supplier the number and date matched when the
// memo was entered, nil for none.
type Citation struct {
	Number    string
	Date      *time.Time
	InvoiceID *uuid.UUID
	// DocumentNumber is the matched invoice's document number, read with
	// it; "" with no InvoiceID.
	DocumentNumber string
}

// The bounds of what a credit memo keeps of its document
// (supplier_invoice_credit_reason, supplier_invoice_citation): a document
// citing more invoices, or a longer number or code, is not read (a hold
// for a person); a longer reason's words are kept shortened.
const (
	maxCitations      = 100
	maxCitedNumber    = 200
	maxReasonCode     = 100
	maxReasonText     = 1000
	clippedTextSuffix = "…"
)

// stated is what an e-invoice credit note states that its credit memo
// keeps: its reason and the invoices it cites.
type stated struct {
	credit    *CreditReason
	citations []Citation
}

// reason is the reason's columns, null without one (and its words null
// when it has none).
func (s stated) reason() (code, text *string) {
	if s.credit == nil {
		return nil, nil
	}
	code = &s.credit.Code
	if s.credit.Text != "" {
		text = &s.credit.Text
	}
	return code, text
}

// storedReason is the reason's columns as read.
type storedReason struct{ code, text *string }

func (r storedReason) reason() *CreditReason {
	if r.code == nil {
		return nil
	}
	out := &CreditReason{Code: *r.code}
	if r.text != nil {
		out.Text = *r.text
	}
	return out
}

// citationDate is a citation's date as invoiceSelect's JSON writes a
// date: YYYY-MM-DD.
type citationDate struct{ time.Time }

func (d *citationDate) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("payables: a citation's date: %w", err)
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return fmt.Errorf("payables: a citation's date: %w", err)
	}
	d.Time = t
	return nil
}

// storedCitations are an invoice's citations as invoiceSelect reads them,
// in position order.
type storedCitations []struct {
	Number         string        `json:"number"`
	Date           *citationDate `json:"date"`
	InvoiceID      *uuid.UUID    `json:"invoice_id"`
	DocumentNumber *string       `json:"document_number"`
}

func (s storedCitations) citations() []Citation {
	if len(s) == 0 {
		return nil
	}
	out := make([]Citation, len(s))
	for i, c := range s {
		out[i] = Citation{Number: c.Number, InvoiceID: c.InvoiceID}
		if c.DocumentNumber != nil {
			out[i].DocumentNumber = *c.DocumentNumber
		}
		if c.Date != nil {
			d := c.Date.Time
			out[i].Date = &d
		}
	}
	return out
}

// putCitations writes a new invoice's citations, numbered from 1 in their
// order; none for an invoice citing none.
func putCitations(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, cs []Citation) error {
	if len(cs) == 0 {
		return nil
	}
	numbers, dates, invoices := make([]string, len(cs)), make([]*time.Time, len(cs)), make([]*uuid.UUID, len(cs))
	for i, c := range cs {
		numbers[i], dates[i], invoices[i] = c.Number, c.Date, c.InvoiceID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number, invoice_date,
			cited_invoice_id)
		SELECT $1, $2, c.position, c.number, c.date, c.invoice FROM unnest($3::text[], $4::date[], $5::uuid[]) WITH ORDINALITY
			AS c(number, date, invoice, position)`, tenant, id, numbers, dates, invoices); err != nil {
		return fmt.Errorf("payables: keep the invoices invoice %s cites: %w", id, err)
	}
	return nil
}

// statedOf is what a credit memo keeps of an e-invoice credit note: its
// reason, a code without words kept without them and words beyond the
// column's bound shortened; a code beyond its bound is not read. An
// invoice keeps none.
func statedOf(d EInvoice) (stated, error) {
	if d.Kind != KindCreditMemo || d.Reason == nil || d.Reason.Code == "" {
		return stated{}, nil
	}
	if len([]rune(d.Reason.Code)) > maxReasonCode {
		return stated{}, unreadable(fmt.Sprintf("the credit note's reason code (BTAE-03) is longer than %d characters", maxReasonCode))
	}
	text := d.Reason.Text
	if r := []rune(text); len(r) > maxReasonText {
		text = string(r[:maxReasonText-1]) + clippedTextSuffix
	}
	return stated{credit: &CreditReason{Code: d.Reason.Code, Text: text}}, nil
}

// citedInvoices matches the invoices a credit memo's document cites to
// the supplier's posted invoices, as creditedInvoice matches the one a
// captured memo prints: by the supplier, the company, the memo's
// currency and the number (supplier references are unique per supplier
// among posted invoices), and by the issue date when the document states
// one (IBT-026). A memo citing one names it as its reference; one citing
// several names none, since no one invoice is what it corrects, the
// citations naming them all for the person who clears it (po:2026-11-21).
// A number that matches no posted invoice holds the memo, the reason
// returned naming the numbers, for a person to check rather than have it
// dropped. A document citing more than maxCitations, or a number longer
// than maxCitedNumber, is not read.
func citedInvoices(ctx context.Context, tx pgx.Tx, in *Input, cited []Citation) ([]Citation, string, error) {
	if len(cited) > maxCitations {
		return nil, "", unreadable(fmt.Sprintf("the credit note cites %d invoices; at most %d are read", len(cited), maxCitations))
	}
	out := make([]Citation, len(cited))
	var unmatched []string
	for i, c := range cited {
		if len([]rune(c.Number)) > maxCitedNumber {
			return nil, "", unreadable(fmt.Sprintf("the credit note cites an invoice number longer than %d characters", maxCitedNumber))
		}
		out[i] = Citation{Number: c.Number, Date: c.Date}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM payables.supplier_invoice WHERE supplier_id = $1 AND supplier_reference_norm = platform.normalize_ref($2)
			AND legal_entity_id = $3 AND currency = $4 AND kind = 'invoice' AND status = 'posted' AND ($5::date IS NULL OR invoice_date = $5)`,
			in.SupplierID, c.Number, in.LegalEntityID, in.Gross.Currency, c.Date).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			unmatched = append(unmatched, c.Number)
		case err != nil:
			return nil, "", fmt.Errorf("payables: find the invoice %s a credit note cites: %w", c.Number, err)
		default:
			out[i].InvoiceID = &id
		}
	}
	if len(out) == 1 {
		in.ReferenceInvoiceID = out[0].InvoiceID
	}
	return out, unmatchedReason(unmatched), nil
}

// unmatchedReason is the hold reason of a credit memo whose cited numbers
// match no posted invoice of the supplier, "" for none.
func unmatchedReason(numbers []string) string {
	switch len(numbers) {
	case 0:
		return ""
	case 1:
		return "The credit memo names the invoice " + numbers[0] + ", which is not a posted invoice of the supplier; enter its reference by hand."
	}
	return "The credit memo names the invoices " + strings.Join(numbers, ", ") + ", which are not posted invoices of the supplier; check them with " +
		"the supplier."
}
