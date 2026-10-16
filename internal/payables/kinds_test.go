package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payables"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestNegativeAmountsRefused: a supplier invoice's gross and tax amounts
// are not negative whatever its kind, which gives the direction (T34): a
// credit memo is entered as one, never as an invoice of negative amounts.
func TestNegativeAmountsRefused(t *testing.T) {
	e := setup(t)
	for name, body := range map[string]string{
		"gross": e.invoice(e.Supplier, "NEG-1", "-100.00", "0.00", accountItem(e.expense, "-100.00")),
		"tax":   e.invoice(e.Supplier, "NEG-2", "100.00", "-19.00", accountItem(e.expense, "119.00")),
	} {
		t.Run(name, func(t *testing.T) {
			c := e.Client
			c.T = t
			c.Problem(422, "payables.negative_amount", "POST", "/v1/supplier-invoices", body, "")
		})
	}
	if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "supplier_invoice_amounts_not_negative"}).Code; got != "payables.negative_amount" {
		t.Errorf("constraint: got %s, want payables.negative_amount", got)
	}
}

// document is an invoice body of a kind in EUR for legal entity 2000;
// extra adds fields such as reference_invoice_id and purchase_order_id.
func (e env) document(kind, ref, gross, tax, extra string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q, "invoice_date": "2026-03-20", "kind": %q,
		"gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"}, "items": [%s]%s}`,
		e.DE, e.Supplier, ref, kind, gross, tax, strings.Join(items, ", "), extra)
}

// TestDocumentKinds enters credit memos and subsequent debits through the
// API: a kind and the posted invoice it corrects, shown, listed by kind
// and published with signed amounts; the rules of each kind refused with
// their codes.
func TestDocumentKinds(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	invoice := e.clean(t, po, "INV-1")
	e.act(t, 200, id(invoice), "match", "")
	e.act(t, 200, id(invoice), "post", "")
	parked := e.clean(t, po, "INV-2")
	ref := fmt.Sprintf(`, "reference_invoice_id": %q`, id(invoice))
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "23.80", "3.80", ref, poItem(po, 1, "2", "EA", "20.00")), "")
	if memo["kind"] != "credit_memo" {
		t.Errorf("credit memo kind: got %v, want credit_memo", memo["kind"])
	}
	if memo["reference_invoice_id"] != id(invoice) {
		t.Errorf("credit memo reference: got %v, want %s", memo["reference_invoice_id"], id(invoice))
	}
	debit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_debit", "SD-1", "5.00", "0.00",
		fmt.Sprintf(`, "purchase_order_id": %q`, po), poItem(po, 1, "0", "EA", "5.00")), "")
	credit, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("subsequent_credit", "SC-1", "3.00", "0.00", ref, poItem(po, 1, "0", "EA", "3.00")), "")
	// The captured events' amounts are signed by the kind.
	signs := map[string]string{id(invoice): "157.80", id(memo): "-23.80", id(debit): "5.00", id(credit): "-3.00"}
	for _, ev := range events(t, e.Tenant, "supplier_invoice_captured") {
		want, ok := signs[ev["supplier_invoice_id"].(string)]
		if !ok {
			continue
		}
		if got := ev["gross_amount"].(map[string]any)["amount"]; got != want {
			t.Errorf("SupplierInvoiceCaptured of a %v: gross got %v, want %s", ev["kind"], got, want)
		}
	}
	page, _ := e.Must(200, "GET", "/v1/supplier-invoices?kind=credit_memo", "", "")
	if items := page["items"].([]any); len(items) != 1 || id(items[0].(map[string]any)) != id(memo) {
		t.Errorf("credit memos listed: got %v, want only %s", items, id(memo))
	}
	for name, c := range map[string]struct{ body, code string }{
		"invoice naming an invoice": {e.document("invoice", "R-1", "23.80", "3.80", ref, poItem(po, 1, "2", "EA", "20.00")), "payables.reference_not_allowed"},
		"reference not posted":      {e.document("credit_memo", "R-2", "23.80", "3.80", fmt.Sprintf(`, "reference_invoice_id": %q`, id(parked)), poItem(po, 1, "2", "EA", "20.00")), "payables.invalid_reference_invoice"},
		"reference unknown":         {e.document("credit_memo", "R-3", "23.80", "3.80", fmt.Sprintf(`, "reference_invoice_id": %q`, uuid.NewString()), poItem(po, 1, "2", "EA", "20.00")), "payables.invalid_reference_invoice"},
		"reference a credit memo":   {e.document("credit_memo", "R-8", "23.80", "3.80", fmt.Sprintf(`, "reference_invoice_id": %q`, id(memo)), poItem(po, 1, "2", "EA", "20.00")), "payables.invalid_reference_invoice"},
		"reference of another supplier": {strings.Replace(e.document("credit_memo", "R-9", "10.00", "0.00", ref, accountItem(e.expense, "10.00")), e.Supplier.String(), e.Vendor.String(), 1),
			"payables.invalid_reference_invoice"},
		"subsequent debit account item with a quantity": {e.document("subsequent_debit", "R-10", "5.00", "0.00", "", accountItem(e.expense, "5.00")), "payables.po_item_required"},
		"unknown kind": {e.document("debit_memo", "R-4", "23.80", "3.80", "", poItem(po, 1, "2", "EA", "20.00")), "payables.unknown_kind"},
		"subsequent debit on an account": {e.document("subsequent_debit", "R-5", "5.00", "0.00", "",
			fmt.Sprintf(`{"account_id": %q, "quantity": {"value": "0", "unit": "EA"}, "amount": {"amount": "5.00", "currency": "EUR"}}`, e.expense)), "payables.po_item_required"},
		"subsequent debit quantity": {e.document("subsequent_debit", "R-6", "5.00", "0.00", "", poItem(po, 1, "2", "EA", "5.00")), "payables.quantity_not_zero"},
		"credit memo no quantity":   {e.document("credit_memo", "R-7", "5.00", "0.00", "", poItem(po, 1, "0", "EA", "5.00")), "payables.invalid_quantity"},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := e.Client
			c2.T = t
			c2.Problem(422, c.code, "POST", "/v1/supplier-invoices", c.body, "")
		})
	}
	t.Run("reference changed and cleared", func(t *testing.T) {
		c := e.Client
		c.T = t
		cleared, tag := c.Must(200, "PATCH", "/v1/supplier-invoices/"+id(memo), `{"reference_invoice_id": null}`, e.tag(t, id(memo)))
		if cleared["reference_invoice_id"] != nil {
			t.Errorf("cleared reference: got %v, want none", cleared["reference_invoice_id"])
		}
		c.Must(200, "PATCH", "/v1/supplier-invoices/"+id(memo), `{"reference_invoice_id": "`+id(invoice)+`"}`, tag)
		c.Problem(422, "payables.reference_not_allowed", "PATCH", "/v1/supplier-invoices/"+id(parked), `{"reference_invoice_id": "`+id(invoice)+`"}`, e.tag(t, id(parked)))
	})
	t.Run("database failures", func(t *testing.T) {
		ref := uuid.MustParse(id(invoice))
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "credit memo with a reference", func(ctx context.Context) error {
			_, err := endpoints(nil).CreateInvoice(ctx, payables.Input{LegalEntityID: e.DE, SupplierID: e.Supplier, SupplierReference: "CM-F",
				InvoiceDate: date("2026-03-20"), Gross: payables.Amount{Value: "11.90", Currency: "EUR"}, Tax: payables.Amount{Value: "1.90", Currency: "EUR"},
				Kind: payables.KindCreditMemo, ReferenceInvoiceID: &ref, Items: []payables.ItemInput{{PurchaseOrderID: &po, POItemNo: ptr(int32(1)),
					Quantity: "1", Unit: "EA", Amount: payables.Amount{Value: "10.00", Currency: "EUR"}}}}, "manual")
			return err
		})
	})
}

func ptr[T any](v T) *T { return &v }
