package einvoicing_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// creditNote is a supplier's PINT AE tax credit note correcting the
// invoice numbered credited ("" naming none): one desk, 250.00 AED with
// 12.50 VAT, on the order's item 10.
func creditNote(number, credited string) []byte {
	d := supplierInvoice(number)
	d.Kind, d.TypeCode, d.Reference = einvoicing.KindTaxCreditNote, einvoicing.TypeTaxCreditNote, credited
	d.Reason = &einvoicing.Reason{Code: "VD", Text: "Damaged desk"}
	d.Lines[0].Quantity, d.Lines[0].Net = "1", 25000
	d.Subtotals[0].Taxable, d.Subtotals[0].Tax, d.Tax = 25000, 1250, 1250
	d.LineTotal, d.TaxExclusive, d.TaxInclusive, d.Payable = 25000, 25000, 26250, 26250
	return ordered(d, "4500000001", "10")
}

// postedInvoice is the supplier's invoice GOI-1 of company 5000, entered
// by hand and posted, which a credit note cites.
func (s *entering) postedInvoice(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
		currency, gross_amount, tax_amount, status, source, journal_entry_id, journal_entry_year, created_by, updated_by)
		VALUES ($1, $2, 'BYHAND-1', $3, $4, 'GOI-1', '2026-03-01', 'AED', 105000, 5000, 'posted', 'manual', gen_random_uuid(), 2026, $5, $5)`,
		s.f.Tenant.ID, id, s.le, s.f.Supplier, s.f.Tenant.Human.ID)
	return id
}

// credited is a message's document as entered for a credit note: its
// status and problem code, and the invoice's kind, status, the invoice it
// names (its supplier reference, "-" for none) and hold reason.
func (s *entering) credited(t *testing.T, message string) (string, string) {
	t.Helper()
	var got, hold string
	pgtest.Scan(t, `SELECT d.status || ' ' || coalesce(d.problem_code, '-') || ' ' || coalesce(i.kind || ' ' || i.status || ' '
			|| coalesce((SELECT r.supplier_reference FROM payables.supplier_invoice r WHERE r.id = i.reference_invoice_id), '-'), '-'),
			coalesce(i.hold_reason, '')
		FROM einvoicing.einvoice d LEFT JOIN payables.supplier_invoice i ON i.id = d.supplier_invoice_id
		WHERE d.tenant_id = $1 AND d.connector_id = $2 AND d.message_id = $3`, []any{s.f.Tenant.ID, s.connector, message}, &got, &hold)
	return got, hold
}

// TestEnterCreditNotes: a fetch enters a valid tax credit note as a
// supplier credit memo naming the invoice it cites, by capture's rule
// (ADR-0061 §6, T65e): a posted invoice of the supplier is its reference
// and it is parked; a number that is no posted invoice of the supplier
// enters it on hold naming the number, its reference left for a person;
// one naming no invoice is held for a person rather than entered as
// correcting none (C4: IBG-03, IBT-025), and one naming several is held
// rather than entered against the first alone, nothing of either
// entered. Run twice, the step enters and holds nothing again.
func TestEnterCreditNotes(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t)
	s.srv.Deliver("CN-1", uaeParticipant, creditNote("CN-1", "GOI-1"))
	s.srv.Deliver("CN-2", uaeParticipant, creditNote("CN-2", "GOI-99"))
	s.srv.Deliver("CN-3", uaeParticipant, creditNote("CN-3", ""))
	ref := "<cac:BillingReference><cac:InvoiceDocumentReference><cbc:ID>GOI-1</cbc:ID></cac:InvoiceDocumentReference></cac:BillingReference>"
	s.srv.Deliver("CN-4", uaeParticipant, []byte(strings.Replace(string(creditNote("CN-4", "GOI-1")), ref, ref+strings.ReplaceAll(ref, "GOI-1", "GOI-2"), 1)))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	for run := range 2 {
		if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil || got.Entered != 2 || got.Held != 2 ||
			got.Refused != 0 {
			t.Fatalf("run %d: got %+v (%v), want CN-1 and CN-2 entered and CN-3 and CN-4 held", run+1, got, err)
		}
	}
	for _, c := range []struct{ message, want, hold string }{
		{"CN-1", "entered - credit_memo parked GOI-1", ""},
		{"CN-2", "entered - credit_memo on_hold -", "names the invoice GOI-99, which is not a posted invoice of the supplier"},
		{"CN-3", "held " + einvoicing.HeldNoInvoice + " -", ""},
		{"CN-4", "held " + einvoicing.HeldSeveralInvoices + " -", ""},
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
	if memos != 2 {
		t.Errorf("credit memos: got %d, want 2, nothing of CN-3 or CN-4", memos)
	}
	var detail string
	pgtest.Scan(t, `SELECT problem_detail FROM einvoicing.einvoice WHERE tenant_id = $1 AND message_id = 'CN-4'`, []any{s.f.Tenant.ID}, &detail)
	if !strings.Contains(detail, "names 2 invoices it corrects (IBT-025), GOI-1, GOI-2") {
		t.Errorf("CN-4's detail: got %q, want the two invoices it names", detail)
	}
}

// TestEnterCreditNoteValidatedBefore: a tax credit note a fetch validated
// before T65e (processed, left received) is entered by the next fetch,
// which counts it, without validating it again (ADR-0061 §4).
func TestEnterCreditNoteValidatedBefore(t *testing.T) {
	s := enteringScene(t)
	s.postedInvoice(t)
	s.srv.Deliver("CN-1", uaeParticipant, creditNote("CN-1", "GOI-1"))
	first, next := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	s.recordAll(t, first)
	pgtest.Exec(t, `UPDATE einvoicing.einvoice SET processed_at = now(), processed_fetch_operation_id = $2, validated_against = $3,
		document_number = message_id, document_kind = 'credit_note', version = version + 1 WHERE tenant_id = $1`, s.f.Tenant.ID, first,
		einvoicing.PINTAERelease)
	got, err := s.e.ProcessEInvoices(context.Background(), s.connector, next, s.within())
	if err != nil || got.Entered != 1 || got.Held != 0 {
		t.Fatalf("the next fetch: got %+v (%v), want CN-1 entered", got, err)
	}
	if got, _ := s.credited(t, "CN-1"); got != "entered - credit_memo parked GOI-1" {
		t.Errorf("CN-1: got %s, want it entered by the next fetch naming GOI-1", got)
	}
	var validated int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'einvoicing.einvoice.validated'`, []any{s.f.Tenant.ID}, &validated)
	if validated != 0 {
		t.Errorf("validations audited: got %d, want none, the credit note processed before", validated)
	}
}
