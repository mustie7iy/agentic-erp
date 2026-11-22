package einvoicing_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// inDollars is the supplier's tax invoice number in USD on the order
// 4500000002 (four desks, 1,000.00 USD with 50.00 VAT), its VAT in AED
// 183.63 (IBT-111) and, unless lineAED is nil, its line's VAT in AED
// (BTAE-08) lineAED and amount with VAT in AED (BTAE-10).
func inDollars(number string, lineAED *money.Minor) []byte {
	d := supplierInvoice(number)
	aed := money.Minor(18363)
	d.Currency, d.TaxCurrency, d.AccountingCurrency, d.AccountingTax = "USD", "AED", "AED", &aed
	if lineAED != nil {
		amount := 367250 + *lineAED
		d.Lines[0].AccountingTax, d.Lines[0].AccountingAmount = lineAED, &amount
	}
	return ordered(d, "4500000002", "10")
}

// TestEnterNotInAED (ADR-0061 §5 C5, As built batch 88; split:T65d,
// po:2026-11-21): a valid tax invoice in USD stating each line's VAT in
// AED (BTAE-08) is entered, the invoice keeping that VAT, the lines' sum,
// as its stated local tax in AED; one without it is held for a person;
// one whose lines' VAT in AED does not sum to its VAT in AED (IBT-111) is
// refused as inconsistent.
func TestEnterNotInAED(t *testing.T) {
	s := enteringScene(t)
	order := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, '4500000002', $3, $4, 'USD', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		s.f.Tenant.ID, order, s.le, s.f.Supplier, s.f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit) VALUES ($1, $2, 10, 'Desk', 4, 'EA', 25000, 1, $3, 'V5', 25000, 1)`,
		s.f.Tenant.ID, order, s.f.Account(t, "5000", "520000"))
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, order)
	stated, wrong := money.Minor(18363), money.Minor(10000)
	s.srv.Deliver("USD-1", uaeParticipant, inDollars("USD-1", &stated))
	s.srv.Deliver("USD-2", uaeParticipant, inDollars("USD-2", nil))
	s.srv.Deliver("USD-3", uaeParticipant, inDollars("USD-3", &wrong))
	op := uuid.Must(uuid.NewV7())
	s.recordAll(t, op)
	for run := range 2 {
		if got, err := s.e.ProcessEInvoices(context.Background(), s.connector, op, s.within()); err != nil || got.Entered != 1 || got.Held != 1 ||
			got.Refused != 1 {
			t.Fatalf("run %d: got %+v (%v), want USD-1 entered, USD-2 held and USD-3 refused", run+1, got, err)
		}
	}
	for message, want := range map[string]string{
		"USD-1": "entered - einvoice parked USD-1 V5 true",
		"USD-2": "held " + einvoicing.HeldNotInAED + " - true",
		"USD-3": "refused " + einvoicing.RefusedInconsistent + " - false",
	} {
		t.Run(message, func(t *testing.T) {
			if got := s.entered(t, message, op); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
	var kept string
	pgtest.Scan(t, `SELECT coalesce(stated_local_tax::text || ' ' || stated_local_currency, '-') FROM payables.supplier_invoice
		WHERE tenant_id = $1 AND supplier_reference = 'USD-1'`, []any{s.f.Tenant.ID}, &kept)
	if kept != "18363 AED" {
		t.Errorf("USD-1's stated local tax: got %s, want 18363 AED, its lines' BTAE-08", kept)
	}
}
