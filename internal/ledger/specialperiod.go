package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Postings into the special periods 13-16 (ADR-0060 §3, T64m1): a year-end
// adjustment is a manual entry, posted directly or parked first, or a
// reversal through the API, that names its special period. The contexts'
// postings never do: only these paths set NewEntry.special or call
// reverseManually, and each checks it here before anything is written.
// postingPeriod then reads the special period's status, FOR SHARE in the
// postings' order, as it reads a regular one's, and the database refuses
// any other posting into a special period (check_entry_period, 000225).

// checkSpecial holds a prepared manual entry naming a special period to
// specialPeriod's rules, and one naming a tax document to checkTaxDocument's;
// one naming neither passes.
func checkSpecial(ctx context.Context, tx pgx.Tx, v prepared, in NewEntry) error {
	if in.special != nil {
		if err := specialPeriod(ctx, tx, v.le, in.PostingDate, *in.special, v.lines, in.TaxDocument != nil); err != nil {
			return err
		}
	}
	return checkTaxDocument(in, v.lines)
}

// specialPeriod refuses a posting dated date into the special period n of
// the fiscal year of date in the legal entity's calendar unless the
// calendar has it (422 ledger.no_fiscal_period), the date is in its dates,
// which are the year's last regular period's (422
// ledger.special_period_date), and its tax codes are allowed
// (specialPeriodTax). Whether the period is open is postingPeriod's to say.
func specialPeriod(ctx context.Context, tx pgx.Tx, le tenancy.Entity, date time.Time, n int16, lines []Line, documented bool) error {
	day := date.Format(time.DateOnly)
	var year int16
	var start, end *time.Time
	err := tx.QueryRow(ctx, `SELECT r.fiscal_year, s.start_date, s.end_date FROM ledger.fiscal_period r
		LEFT JOIN ledger.fiscal_period s ON s.tenant_id = r.tenant_id AND s.calendar_id = r.calendar_id AND s.fiscal_year = r.fiscal_year
			AND s.period = $3 AND s.is_special
		WHERE r.calendar_id = $1 AND NOT r.is_special AND $2 BETWEEN r.start_date AND r.end_date`, le.FiscalCalendarID, date, n).Scan(&year, &start, &end)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return noFiscalPeriod("posting_date", "No fiscal period of the legal entity covers "+day+
			"; a fiscal year is set up by a number range for it in the configuration.")
	case err != nil:
		return fmt.Errorf("ledger: special period %d of calendar %s on %s: %w", n, le.FiscalCalendarID, day, err)
	case start == nil:
		return noFiscalPeriod("fiscal_period", fmt.Sprintf("The fiscal calendar has no special period %d in fiscal year %d.", n, year))
	case date.Before(*start) || date.After(*end):
		return problem.Unprocessable("ledger.special_period_date", fmt.Sprintf("Special period %d/%d takes postings dated %s to %s, the year's last regular period, whose dates it shares; this one is dated %s.",
			year, n, start.Format(time.DateOnly), end.Format(time.DateOnly), day),
			problem.FieldError{Field: "posting_date", Code: "special_period_date", Message: "a date from " + start.Format(time.DateOnly) + " to " + end.Format(time.DateOnly)})
	}
	return specialPeriodTax(le, fmt.Sprintf("%d/%d", year, n), lines, documented)
}

// taxDocumentCountry is the country whose companies' year-end adjustments
// may carry tax codes: the United Arab Emirates, whose tax specialist
// answered for the VAT 201 (ADR-0060 §3, question 2; C10, 2026-11-09). A
// company's country decides, as its tax codes and its returns are its
// country's; Canada's GST34 is not answered.
const taxDocumentCountry = "AE"

// specialPeriodTax refuses a posting into the special period (year/number)
// with a tax code on a line (422 ledger.special_period_tax_code) unless
// its company is in the UAE and the entry rests on a tax invoice or tax
// credit note (documented): VAT follows the tax documents, not the
// accounting date, and accruals, provisions and reclassifications carry
// none (ADR-0060 §3; the UAE tax specialist, C10). Tax returns select
// their items by tax date, which is then the document's (NewEntry.taxDay).
// Every other country keeps the refusal until its tax specialist answers
// (question 2).
func specialPeriodTax(le tenancy.Entity, period string, lines []Line, documented bool) error {
	i := slices.IndexFunc(lines, func(l Line) bool { return l.TaxCode != nil })
	if i < 0 || le.CountryCode == taxDocumentCountry && documented {
		return nil
	}
	why, hint := "a posting into a special period carries no tax code in a company of country "+le.CountryCode+
		" (ADR-0060 question 2: refused until its tax specialist confirms how year-end adjustments enter its returns)", "no tax code in a special period"
	if le.CountryCode == taxDocumentCountry {
		why, hint = "a year-end adjustment carries VAT only when it rests on a tax invoice or tax credit note, named in tax_document (a reversal's is "+
			"its original's); accruals, provisions and reclassifications carry none (ADR-0060 §3)", "a tax code only with a tax document"
	}
	return problem.Unprocessable("ledger.special_period_tax_code", fmt.Sprintf("Line %d carries tax code %s in special period %s: %s.", i+1, *lines[i].TaxCode, period, why),
		problem.FieldError{Field: fmt.Sprintf("lines[%d].tax_code", i), Code: "special_period_tax_code", Message: hint})
}

// TaxDocument is the tax invoice or tax credit note a year-end adjustment
// carrying VAT rests on (ADR-0060 §3; the UAE tax specialist, C10): what
// the entry says of it. The ERP holds no such document to check it
// against. Its date is the entry's tax date.
type TaxDocument struct {
	Kind   string // tax_invoice or tax_credit_note
	Number string
	Date   time.Time
}

// The kinds of tax document (TaxDocument.Kind).
const (
	TaxInvoice    = "tax_invoice"
	TaxCreditNote = "tax_credit_note"
)

// checkTaxDocument refuses a manual entry's tax document (422
// ledger.invalid_tax_document) of an unknown kind, on an entry that does
// not need one (it names no special period, or no line carries a tax
// code), or beside a tax date that is not the document's date: the
// document decides the tax date, so nothing else may say another.
func checkTaxDocument(in NewEntry, lines []Line) error {
	d := in.TaxDocument
	if d == nil {
		return nil
	}
	invalid := func(detail, field, code, message string) error {
		return problem.Unprocessable("ledger.invalid_tax_document", detail, problem.FieldError{Field: field, Code: code, Message: message})
	}
	switch {
	case d.Kind != TaxInvoice && d.Kind != TaxCreditNote:
		return invalid("A tax document is a tax invoice or a tax credit note.", "tax_document.kind", "unknown", TaxInvoice+" or "+TaxCreditNote)
	case in.special == nil || !taxed(lines):
		return invalid("A tax document is named by a year-end adjustment that carries VAT: an entry with fiscal_period and a tax code on a line (ADR-0060 §3).",
			"tax_document", "not_expected", "only with fiscal_period and a tax code on a line")
	case in.TaxDate != nil && !in.TaxDate.Equal(d.Date):
		return invalid(fmt.Sprintf("The tax date %s is not the tax document's date, %s: the document's date is the entry's tax date.",
			in.TaxDate.Format(time.DateOnly), d.Date.Format(time.DateOnly)), "tax_date", "not_document_date", "the tax document's date, or none")
	}
	return nil
}

// columns are the document's kind, number and date as an entry's columns
// hold them: all null without one.
func (d *TaxDocument) columns() (kind, number *string, date *time.Time) {
	if d == nil {
		return nil, nil, nil
	}
	return &d.Kind, &d.Number, &d.Date
}

// taxDocumentOf is the document an entry's columns hold, or nil; the
// database keeps the three together (journal_entry_tax_document,
// parked_entry_tax_document).
func taxDocumentOf(kind, number *string, date *time.Time) *TaxDocument {
	if kind == nil {
		return nil
	}
	return &TaxDocument{Kind: *kind, Number: *number, Date: *date}
}

// taxDocumentToAPI is the document as the API shows it.
func taxDocumentToAPI(d *TaxDocument) gen.OptTaxDocumentReference {
	if d == nil {
		return gen.OptTaxDocumentReference{}
	}
	return gen.NewOptTaxDocumentReference(gen.TaxDocumentReference{Kind: d.Kind, Number: d.Number, Date: d.Date})
}

// taxDocumentIn is the document a create request names, or nil.
func taxDocumentIn(o gen.OptTaxDocumentReference) *TaxDocument {
	v, ok := o.Get()
	if !ok {
		return nil
	}
	return &TaxDocument{Kind: v.Kind, Number: v.Number, Date: v.Date}
}

// taxDocumentChange maps a merge patch's tax_document: absent → nil, null
// → a pointer to nil (no document), a value → a pointer to it.
func taxDocumentChange(o gen.OptNilParkedJournalEntryUpdateTaxDocument) **TaxDocument {
	if !o.Set {
		return nil
	}
	var d *TaxDocument
	if !o.Null {
		d = &TaxDocument{Kind: o.Value.Kind, Number: o.Value.Number, Date: o.Value.Date}
	}
	return &d
}

// specialOf is the special period a request names, or nil.
func specialOf(o gen.OptInt32) *int16 {
	v, ok := o.Get()
	if !ok {
		return nil
	}
	n := int16(v) // 13 to 16: the contract's bounds, which ogen validates
	return &n
}

// specialChange maps a merge patch's fiscal_period: absent → nil, null →
// a pointer to nil (the regular period again), a value → a pointer to it.
func specialChange(o gen.OptNilInt32) **int16 {
	if !o.Set {
		return nil
	}
	var v *int16
	if !o.Null {
		n := int16(o.Value) // 13 to 16, as specialOf
		v = &n
	}
	return &v
}
