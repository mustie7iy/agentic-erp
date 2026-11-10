package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// The tax document of a year-end adjustment (ADR-0060 §3, C10; migrations
// 000247_tax_document and 000248_validate_tax_document): an entry names a
// tax invoice or tax credit note whole, kind, number and date, or not at
// all; a posted entry's tax date is the document's date, and a parked
// entry's tax date, when it has one, is too.

// taxDocumentCases are a document as an entry names it and the constraint
// that refuses it (name), or none; taxDate is the entry's tax date.
var taxDocumentCases = []struct {
	name                        string
	kind, number, date, taxDate any
	refused                     bool
}{
	{"none", nil, nil, nil, "2026-12-31", false},
	{"a tax invoice", "tax_invoice", "TI-1", "2026-08-15", "2026-08-15", false},
	{"a tax credit note", "tax_credit_note", "TCN 7/2026", "2026-08-15", "2026-08-15", false},
	{"another kind", "receipt", "R-1", "2026-08-15", "2026-08-15", true},
	{"without its number", "tax_invoice", nil, "2026-08-15", "2026-08-15", true},
	{"without its date", "tax_invoice", "TI-1", nil, "2026-08-15", true},
	{"a number and date of no kind", nil, "TI-1", "2026-08-15", "2026-08-15", true},
	{"a blank number", "tax_invoice", " ", "2026-08-15", "2026-08-15", true},
	{"a number of 65 characters", "tax_invoice", "TI-45678901234567890123456789012345678901234567890123456789012345", "2026-08-15", "2026-08-15", true},
	{"another tax date", "tax_invoice", "TI-1", "2026-08-15", "2026-12-31", true},
}

// TestJournalEntryTaxDocument: a posted entry's tax document is whole, of
// a known kind, and dates the entry's tax.
func TestJournalEntryTaxDocument(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range append(taxDocumentCases, struct {
		name                        string
		kind, number, date, taxDate any
		refused                     bool
	}{"no tax date", "tax_invoice", "TI-1", "2026-08-15", nil, true}) {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				je := id()
				if err := exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
					document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by,
					tax_date, tax_document_kind, tax_document_number, tax_document_date)
					VALUES ($1,2026,$2,$3,$4,12,'SA',$5,'2026-12-31','2026-12-31','USD','USD','EUR',1,0.9,'ledger',$6,$7,$8,$9,$10)`,
					f.tenant, je, f.entity, f.ledger, je.String()[24:], f.actor, c.taxDate, c.kind, c.number, c.date); err != nil {
					return err
				}
				return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
					posting_date, account_id, amount_tc, amount_lc, amount_gc) VALUES ($1,2026,$2,1,$3,$4,12,'2026-12-31',$5,10,10,9), ($1,2026,$2,2,$3,$4,12,'2026-12-31',$6,-10,-10,-9)`,
					f.tenant, je, f.entity, f.ledger, f.expense, f.bank)
			})
			want := ""
			if c.refused {
				want = "journal_entry_tax_document"
			}
			expect(t, err, want)
		})
	}
}

// TestParkedEntryTaxDocument: a parked entry's tax document is whole and
// of a known kind, and its tax date, when it has one, is the document's.
func TestParkedEntryTaxDocument(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range taxDocumentCases {
		t.Run(c.name, func(t *testing.T) {
			pe := f.park(t, "human")
			want := ""
			if c.refused {
				want = "parked_entry_tax_document"
			}
			expect(t, f.change(t, pe, "tax_date = $2, tax_document_kind = $3, tax_document_number = $4, tax_document_date = $5",
				c.taxDate, c.kind, c.number, c.date), want)
		})
	}
	t.Run("no tax date", func(t *testing.T) {
		pe := f.park(t, "human")
		expect(t, f.change(t, pe, "tax_date = NULL, tax_document_kind = 'tax_invoice', tax_document_number = 'TI-1', tax_document_date = '2026-08-15'"), "")
	})
}
