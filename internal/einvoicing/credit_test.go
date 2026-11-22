package einvoicing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// creditNote is a supplier's PINT AE tax credit note for the reason code
// reason (BTAE-03; illustrative but for VD, a volume discount) citing the
// invoices cited (IBG-03, none for none): one desk, 250.00 AED with 12.50
// VAT, on the order's item 10.
func creditNote(number, reason string, cited ...einvoicing.Cited) []byte {
	return ordered(creditNoteOf(number, reason, cited...), "4500000001", "10")
}

// creditNoteOf is creditNote's document, naming no order.
func creditNoteOf(number, reason string, cited ...einvoicing.Cited) einvoicing.Document {
	d := supplierInvoice(number)
	d.Kind, d.TypeCode, d.Cited, d.OrderReference = einvoicing.KindTaxCreditNote, einvoicing.TypeTaxCreditNote, cited, ""
	d.Reason = &einvoicing.Reason{Code: reason, Text: "Damaged desk"}
	d.Lines[0].Quantity, d.Lines[0].Net = "1", 25000
	d.Subtotals[0].Taxable, d.Subtotals[0].Tax, d.Tax = 25000, 1250, 1250
	d.LineTotal, d.TaxExclusive, d.TaxInclusive, d.Payable = 25000, 25000, 26250, 26250
	return d
}

// rebateNote is a volume discount's tax credit note naming no order nor
// invoice, of the seller whose TRN is trn: 250.00 AED with 12.50 VAT.
func rebateNote(number, trn string) []byte {
	d := creditNoteOf(number, einvoicing.ReasonVolumeDiscount)
	d.Seller.TRN, d.Seller.Endpoint = trn, trn[:10]
	d.Reason.Text, d.Lines[0].Description = "Volume discount, first quarter", "Volume discount"
	return ordered(d, "", "")
}

// rebateAccount gives company 5000 the account of discounts received
// (account determination cash_discount), 400000, and returns it.
func (s *entering) rebateAccount(t *testing.T) uuid.UUID {
	t.Helper()
	account := s.f.Account(t, "5000", "400000")
	pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by)
		VALUES ($1, $2, 'cash_discount', $3, $4)`, s.f.Tenant.ID, s.le, account, s.f.Tenant.Human.ID)
	return account
}

// citing is the invoice numbered number, and dated date ("" for none).
func citing(number, date string) einvoicing.Cited {
	c := einvoicing.Cited{Number: number}
	if date != "" {
		d, _ := time.Parse(time.DateOnly, date) // a test's own date
		c.Date = &d
	}
	return c
}

// postedInvoice is the supplier's invoice reference of company 5000,
// dated 2026-03-01, entered by hand and posted, which a credit note cites.
func (s *entering) postedInvoice(t *testing.T, reference string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
		currency, gross_amount, tax_amount, status, source, journal_entry_id, journal_entry_year, created_by, updated_by)
		VALUES ($1, $2, 'BYHAND-' || $6, $3, $4, $6, '2026-03-01', 'AED', 105000, 5000, 'posted', 'manual', gen_random_uuid(), 2026, $5, $5)`,
		s.f.Tenant.ID, id, s.le, s.f.Supplier, s.f.Tenant.Human.ID, reference)
	return id
}

// credited is a message's document as entered for a credit note: entered
// is its status and problem code, then the invoice's kind, status and the
// invoice it names (its supplier reference, "-" for none), its reason
// code and the invoices it cites, each number with the supplier reference
// it matched ("-" for none); hold is the invoice's hold reason.
func (s *entering) credited(t *testing.T, message string) (entered, hold string) {
	t.Helper()
	pgtest.Scan(t, `SELECT d.status || ' ' || coalesce(d.problem_code, '-') || ' ' || coalesce(i.kind || ' ' || i.status || ' '
			|| coalesce((SELECT r.supplier_reference FROM payables.supplier_invoice r WHERE r.id = i.reference_invoice_id), '-')
			|| ' ' || coalesce(i.credit_reason_code, '-') || ' ['
			|| coalesce((SELECT string_agg(c.invoice_number || ':' || coalesce(r.supplier_reference, '-'), ',' ORDER BY c.position)
				FROM payables.supplier_invoice_citation c LEFT JOIN payables.supplier_invoice r ON r.id = c.cited_invoice_id
				WHERE c.supplier_invoice_id = i.id), '') || ']', '-'),
			coalesce(i.hold_reason, '')
		FROM einvoicing.einvoice d LEFT JOIN payables.supplier_invoice i ON i.id = d.supplier_invoice_id
		WHERE d.tenant_id = $1 AND d.connector_id = $2 AND d.message_id = $3`, []any{s.f.Tenant.ID, s.connector, message}, &entered, &hold)
	return entered, hold
}

// TestEnterCreditNotes: a fetch enters a valid tax credit note as one
// supplier credit memo citing every invoice it names, keeping its reason
// (ADR-0061 §6, T65e, As built batch 88; po:2026-11-21): one citing a
// posted invoice of the supplier names it as its reference and is parked;
// a number that is no posted invoice of the supplier enters it on hold
// naming the number, as does a date (IBT-026) that is not that invoice's;
// one citing several invoices names none as its reference, citing them
// all, on hold when one is not posted; a volume discount (VD) citing none
// is entered without a reference (IBR-055-AE), any other citing none is
// held for a person (C4: IBG-03, IBT-025), nothing of it entered; one
// naming GOI-1 twice beside a blank reference names GOI-1 once; a reason
// code beyond 100 characters is not read, a hold; two spellings of one
// invoice's number cite it once. Run
// twice, the step enters and holds nothing again.
func TestEnterCreditNotes(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t, "GOI-1")
	s.postedInvoice(t, "GOI-2")
	notes := map[string][]byte{
		"CN-1": creditNote("CN-1", "DL", citing("GOI-1", "")),
		"CN-2": creditNote("CN-2", "DL", citing("GOI-99", "")),
		"CN-3": creditNote("CN-3", "DL"),
		"CN-4": creditNote("CN-4", "DL", citing("GOI-1", ""), citing("GOI-98", "")),
		"CN-5": creditNote("CN-5", "DL", citing(" ", ""), citing("GOI-1", ""), citing("GOI-1", "2026-03-01")),
		"CN-6": creditNote("CN-6", einvoicing.ReasonVolumeDiscount),
		"CN-7": creditNote("CN-7", "DL", citing("GOI-1", "2026-03-01"), citing("GOI-2", "2026-03-01")),
		"CN-8": creditNote("CN-8", "DL", citing("GOI-1", "2026-03-02")),
		"CN-9": creditNote("CN-9", strings.Repeat("V", 101), citing("GOI-1", "")),
		"CN-A": creditNote("CN-A", "DL", citing("GOI-1", ""), citing("goi 1", "")),
	}
	for m, doc := range notes {
		s.srv.Deliver(m, uaeParticipant, doc)
	}
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	for run := range 2 {
		if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil || got.Entered != 8 || got.Held != 2 ||
			got.Refused != 0 {
			t.Fatalf("run %d: got %+v (%v), want all but CN-3 and CN-9 entered, and those held", run+1, got, err)
		}
	}
	for _, c := range []struct{ message, want, hold string }{
		{"CN-1", "entered - credit_memo parked GOI-1 DL [GOI-1:GOI-1]", ""},
		{"CN-2", "entered - credit_memo on_hold - DL [GOI-99:-]", "names the invoice GOI-99, which is not a posted invoice of the supplier"},
		{"CN-3", "held " + einvoicing.HeldNoInvoice + " -", ""},
		{"CN-4", "entered - credit_memo on_hold - DL [GOI-1:GOI-1,GOI-98:-]", "names the invoice GOI-98, which is not a posted invoice of the supplier, beside the others"},
		{"CN-5", "entered - credit_memo parked GOI-1 DL [GOI-1:GOI-1]", ""},
		{"CN-6", "entered - credit_memo parked - VD []", ""},
		{"CN-7", "entered - credit_memo parked - DL [GOI-1:GOI-1,GOI-2:GOI-2]", ""},
		{"CN-8", "entered - credit_memo on_hold - DL [GOI-1:-]", "names the invoice GOI-1, which is not a posted invoice"},
		{"CN-9", "held payables.unreadable_document -", ""},
		{"CN-A", "entered - credit_memo parked GOI-1 DL [GOI-1:GOI-1]", ""},
	} {
		t.Run(c.message, func(t *testing.T) {
			got, hold := s.credited(t, c.message)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			if !strings.Contains(hold, c.hold) {
				t.Errorf("hold reason: got %q, want it to say %q", hold, c.hold)
			}
		})
	}
	var memos int
	pgtest.Scan(t, `SELECT count(*) FROM payables.supplier_invoice WHERE tenant_id = $1 AND kind = 'credit_memo'`, []any{s.f.Tenant.ID}, &memos)
	if memos != 8 {
		t.Errorf("credit memos: got %d, want 8, nothing of CN-3 or CN-9", memos)
	}
	var detail string
	pgtest.Scan(t, `SELECT problem_detail FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'CN-3'`, []any{s.f.Tenant.ID}, &detail)
	if !strings.Contains(detail, "not for a volume discount (BTAE-03 VD)") {
		t.Errorf("CN-3's detail: got %q, want it to say a volume discount may name none", detail)
	}
	var dated string
	pgtest.Scan(t, `SELECT coalesce(string_agg(c.invoice_number || ' ' || coalesce(c.invoice_date::text, '-'), ', ' ORDER BY c.position), '')
		FROM payables.supplier_invoice_citation c JOIN payables.supplier_invoice i ON i.id = c.supplier_invoice_id
		WHERE i.tenant_id = $1 AND i.supplier_reference = 'CN-5'`, []any{s.f.Tenant.ID}, &dated)
	if dated != "GOI-1 2026-03-01" {
		t.Errorf("CN-5's citation: got %q, want GOI-1 with the date its repeat states", dated)
	}
}

// TestEnterCreditNoteValidatedBefore: a tax credit note a fetch validated
// before T65e (processed, left received) is entered by the next fetch,
// which counts it, without validating it again (ADR-0061 §4).
func TestEnterCreditNoteValidatedBefore(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t, "GOI-1")
	s.srv.Deliver("CN-1", uaeParticipant, creditNote("CN-1", "DL", citing("GOI-1", "")))
	first, next := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	s.recordAll(t, first)
	pgtest.Exec(t, `UPDATE einvoicing.einvoice SET processed_at = now(), processed_fetch_operation_id = $2, validated_against = $3,
		document_number = message_id, document_kind = 'credit_note', version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID, first,
		einvoicing.PINTAERelease)
	got, err := s.e.ProcessEInvoices(context.Background(), s.connector, next, s.within())
	if err != nil || got.Entered != 1 || got.Held != 0 {
		t.Fatalf("the next fetch: got %+v (%v), want CN-1 entered", got, err)
	}
	if got, _ := s.credited(t, "CN-1"); got != "entered - credit_memo parked GOI-1 DL [GOI-1:GOI-1]" {
		t.Errorf("CN-1: got %s, want it entered by the next fetch naming GOI-1", got)
	}
	var validated int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.validated'`, []any{s.f.Tenant.ID}, &validated)
	if validated != 0 {
		t.Errorf("validations audited: got %d, want none, the credit note processed before", validated)
	}
}

// TestEnterCreditNoteDatabaseFailures: a failure of any statement while a
// credit note citing two invoices is entered (matching each, keeping its
// citations) fails the step rather than holding the document, which the
// next try enters.
func TestEnterCreditNoteDatabaseFailures(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t, "GOI-1")
	s.postedInvoice(t, "GOI-2")
	s.srv.Deliver("CN-1", uaeParticipant, creditNote("CN-1", "DL", citing("GOI-1", "2026-03-01"), citing("GOI-2", "")))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	everyFailure(t, s.scene, func(in einvoicing.Within) error {
		_, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, in)
		return err
	})
	if got, _ := s.credited(t, "CN-1"); got != "entered - credit_memo parked - DL [GOI-1:GOI-1,GOI-2:GOI-2]" {
		t.Errorf("CN-1 after the failures: got %s, want it entered citing both", got)
	}
}

// TestEnterVolumeDiscountWithoutOrder: a volume discount's tax credit note
// naming no purchase order is entered as a supplier credit memo without an
// order or a reference (ADR-0061 As built batch 89; po:2026-11-21): the
// supplier the one of the company holding the seller's TRN, its line an
// item of 1 EA on the account of discounts received, its tax as stated,
// on hold for the accounts payable clerk to choose its code among the two
// of its category and rate (V5, VB5: capturedTax's rule) and post; a
// company without that account holds it
// (payables.capture_needs_account), as does a seller no supplier of the
// company holds the TRN of (payables.einvoice_seller_unknown); a credit
// note for another reason naming no order is held as before
// (payables.capture_needs_order). Run twice, the step enters and holds
// nothing again.
func TestEnterVolumeDiscountWithoutOrder(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t, "GOI-1")
	s.srv.Deliver("VD-0", uaeParticipant, rebateNote("VD-0", supplierTRN))
	s.srv.Deliver("DL-1", uaeParticipant, ordered(creditNoteOf("DL-1", "DL", citing("GOI-1", "")), "", ""))
	first := uuid.Must(uuid.NewV7())
	s.recordAll(t, first)
	if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, first, s.within()); err != nil || got.Entered != 0 || got.Held != 2 {
		t.Fatalf("without the account: got %+v (%v), want both held", got, err)
	}
	account := s.rebateAccount(t)
	s.srv.Deliver("VD-1", uaeParticipant, rebateNote("VD-1", supplierTRN))
	s.srv.Deliver("VD-2", uaeParticipant, rebateNote("VD-2", "100999999900003"))
	next := uuid.Must(uuid.NewV7())
	s.recordAll(t, next)
	for run := range 2 {
		if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, next, s.within()); err != nil || got.Entered != 1 || got.Held != 1 {
			t.Fatalf("run %d: got %+v (%v), want VD-1 entered and VD-2 held", run+1, got, err)
		}
	}
	for _, c := range []struct{ message, want, hold string }{
		{"VD-0", "held payables.capture_needs_account -", ""},
		{"DL-1", "held payables.capture_needs_order -", ""},
		{"VD-1", "entered - credit_memo on_hold - VD []", "5% in VAT category S, which codes V5, VB5 charge"},
		{"VD-2", "held payables.einvoice_seller_unknown -", ""},
	} {
		t.Run(c.message, func(t *testing.T) {
			got, hold := s.credited(t, c.message)
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			if !strings.Contains(hold, c.hold) {
				t.Errorf("hold reason: got %q, want it to say %q", hold, c.hold)
			}
		})
	}
	var memo string
	pgtest.Scan(t, `SELECT i.supplier_id::text || ' ' || coalesce(i.purchase_order_id::text, '-') || ' ' || i.gross_amount || ' ' || i.tax_amount || ' '
			|| string_agg(it.quantity::numeric::text || ' ' || it.uom_code || ' ' || it.amount || ' ' || coalesce(it.tax_code, '-') || ' '
				|| coalesce(it.purchase_order_id::text, '-') || ' ' || (it.account_id = $3) || ' ' || coalesce(it.description, '-'), ', ' ORDER BY it.item_no)
		FROM payables.supplier_invoice i JOIN payables.supplier_invoice_item it ON it.supplier_invoice_id = i.id
		WHERE i.tenant_id = $1 AND i.supplier_reference = $2 GROUP BY i.tenant_id, i.id`, []any{s.f.Tenant.ID, "VD-1", account}, &memo)
	if want := s.f.Supplier.String() + " - 26250 1250 1.000000 EA 25000 - - true Volume discount"; memo != want {
		t.Errorf("VD-1's memo: got %s, want %s", memo, want)
	}
	var detail string
	pgtest.Scan(t, `SELECT problem_detail FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'VD-2'`, []any{s.f.Tenant.ID}, &detail)
	if !strings.Contains(detail, "ending 0003,") {
		t.Errorf("VD-2's detail: got %q, want it to name the TRN's last four", detail)
	}
	if strings.Contains(detail, "100999999900003") {
		t.Errorf("VD-2's detail: got %q, want no TRN in clear", detail)
	}
}

// TestEnterVolumeDiscountDatabaseFailures: a failure of any statement
// while a volume discount naming no order is entered (finding the seller's
// supplier, the account of discounts received) fails the step rather than
// holding the document, which the next try enters.
func TestEnterVolumeDiscountDatabaseFailures(t *testing.T) {
	s := enteringScene(t)
	s.rebateAccount(t)
	s.srv.Deliver("VD-1", uaeParticipant, rebateNote("VD-1", supplierTRN))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	everyFailure(t, s.scene, func(in einvoicing.Within) error {
		_, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, in)
		return err
	})
	if got, _ := s.credited(t, "VD-1"); got != "entered - credit_memo on_hold - VD []" {
		t.Errorf("VD-1 after the failures: got %s, want it entered, on hold for its code", got)
	}
}
