package payables_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// memo is the extraction of a credit memo on PO-1 crediting 2 EA of the
// invoice INV-1, its amounts printed with the sign given.
func memo(number, sign string) *capture.Extraction {
	return &capture.Extraction{DocumentKind: "credit_memo", CreditedInvoiceNumber: "INV-1", VendorName: "Supplier", InvoiceNumber: number,
		InvoiceDate: "2026-03-12", PurchaseOrder: "PO-1", Currency: "EUR",
		Lines:     []capture.Line{{Description: "Paper", Quantity: "2", Amount: sign + "24.00", POLine: 1}},
		TaxAmount: sign + "4.56", GrossAmount: sign + "28.56", Confidence: "high"}
}

// TestCaptureCreditMemo: a document the extraction reads as a credit memo
// is entered as one, whether it prints its amounts negative or not (the
// kind gives the direction, amounts are entered unsigned), naming the
// posted invoice of the supplier whose number it prints; it then matches
// against what was invoiced. A credited invoice that is not a posted
// invoice of the supplier holds the memo for its reference to be entered
// by hand; an invoice printing negative amounts, or a credit memo printing
// both signs, is not captured.
func TestCaptureCreditMemo(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "142.80", "22.80", poItem(po, 1, "10", "EA", "120.00")), "")
	e.posted(t, invoice)
	run := func(x *capture.Extraction, doc string) (map[string]any, error) {
		var res gen.CaptureSupplierInvoiceRes
		err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			res, err = endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString(doc)},
				gen.CaptureSupplierInvoiceParams{})
			return err
		})
		if err != nil {
			return nil, err
		}
		v, _ := e.Must(200, "GET", res.(*gen.OperationHeaders).Response.Resource.Value, "", "")
		return v, nil
	}
	for number, sign := range map[string]string{"CM-A": "-", "CM-B": ""} {
		t.Run("printed "+sign+"28.56", func(t *testing.T) {
			e := e.in(t)
			v, err := run(memo(number, sign), "%PDF-1 memo "+sign)
			if err != nil {
				t.Fatal(err)
			}
			if v["kind"] != "credit_memo" {
				t.Errorf("captured kind: got %v, want credit_memo", v["kind"])
			}
			if v["reference_invoice_id"] != id(invoice) {
				t.Errorf("captured reference: got %v, want the invoice INV-1 %s", v["reference_invoice_id"], id(invoice))
			}
			if got := v["gross_amount"].(map[string]any)["amount"]; got != "28.56" {
				t.Errorf("captured gross: got %v, want 28.56", got)
			}
			if got := v["items"].([]any)[0].(map[string]any)["amount"].(map[string]any)["amount"]; got != "24.00" {
				t.Errorf("captured item amount: got %v, want 24.00", got)
			}
			if v["status"] != "parked" {
				t.Errorf("captured status: got %v, want parked", v["status"])
			}
			if m := e.act(t, 200, id(v), "match", ""); m["is_clean"] != true {
				t.Errorf("match of the captured memo: got issues %v, want clean", m["issues"])
			}
		})
	}
	unknown := memo("CM-3", "")
	unknown.CreditedInvoiceNumber = "INV-9"
	v, err := run(unknown, "%PDF-1 memo of an unknown invoice")
	if err != nil {
		t.Fatal(err)
	}
	if v["status"] != "on_hold" {
		t.Errorf("memo of an unknown invoice, status: got %v, want on_hold", v["status"])
	}
	if v["reference_invoice_id"] != nil {
		t.Errorf("memo of an unknown invoice, reference: got %v, want none", v["reference_invoice_id"])
	}
	if got, _ := v["hold_reason"].(string); got != "The credit memo names the invoice INV-9, which is not a posted invoice of the supplier; enter its reference by hand." {
		t.Errorf("memo of an unknown invoice, hold reason: got %q", got)
	}
	unsure := memo("CM-6", "")
	unsure.CreditedInvoiceNumber, unsure.Confidence = "INV-9", "low"
	if v, err = run(unsure, "%PDF-1 unsure memo of an unknown invoice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := v["hold_reason"].(string); !strings.HasPrefix(got, "Low extraction confidence") || !strings.Contains(got, "INV-9") {
		t.Errorf("unsure memo of an unknown invoice, hold reason: got %q, want both reasons", got)
	}
	unreferenced := memo("CM-4", "")
	unreferenced.CreditedInvoiceNumber = ""
	if v, err = run(unreferenced, "%PDF-1 memo without a reference"); err != nil {
		t.Fatal(err)
	}
	if v["status"] != "parked" {
		t.Errorf("memo naming no invoice, status: got %v, want parked", v["status"])
	}
	if v["reference_invoice_id"] != nil {
		t.Errorf("memo naming no invoice, reference: got %v, want none", v["reference_invoice_id"])
	}
	mixed := memo("CM-5", "-")
	mixed.TaxAmount = "4.56"
	_, err = run(mixed, "%PDF-1 memo of both signs")
	pgtest.WantErr(t, err, "payables.unreadable_document")
	onInvoice := extraction()
	onInvoice.InvoiceNumber, onInvoice.CreditedInvoiceNumber = "CAP-9", "INV-1"
	_, err = run(onInvoice, "%PDF-1 invoice naming an invoice")
	pgtest.WantErr(t, err, "payables.unreadable_document")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "capture a credit memo", func(ctx context.Context) error {
		_, err := endpoints(extractor{e: memo("CM-F", "-")}).CaptureInvoice(ctx, e.DE, []byte("%PDF failing memo"))
		return err
	})
}
