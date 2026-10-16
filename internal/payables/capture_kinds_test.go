package payables_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/payables"
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

// with is the extraction x changed by change.
func with(x *capture.Extraction, change func(*capture.Extraction)) *capture.Extraction {
	change(x)
	return x
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
	unknownReason := "The credit memo names the invoice INV-9, which is not a posted invoice of the supplier; enter its reference by hand."
	lowConfidence := "Low extraction confidence; check the invoice against the document."
	captured := map[string]struct {
		x         *capture.Extraction
		status    string
		reference bool   // the memo names INV-1
		reason    string // the hold reason
	}{
		"printed negative":           {memo("CM-A", "-"), payables.Parked, true, ""},
		"printed positive":           {memo("CM-B", ""), payables.Parked, true, ""},
		"quantity printed negative":  {with(memo("CM-C", "-"), func(x *capture.Extraction) { x.Lines[0].Quantity = "-2" }), payables.Parked, true, ""},
		"unknown invoice":            {with(memo("CM-D", ""), func(x *capture.Extraction) { x.CreditedInvoiceNumber = "INV-9" }), payables.OnHold, false, unknownReason},
		"unsure, of unknown invoice": {with(memo("CM-E", ""), func(x *capture.Extraction) { x.CreditedInvoiceNumber, x.Confidence = "INV-9", "low" }), payables.OnHold, false, lowConfidence + " " + unknownReason},
		"naming no invoice":          {with(memo("CM-G", ""), func(x *capture.Extraction) { x.CreditedInvoiceNumber = "" }), payables.Parked, false, ""},
	}
	for name, c := range captured {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			v, err := run(c.x, "%PDF-1 "+name)
			if err != nil {
				t.Fatal(err)
			}
			if v["kind"] != "credit_memo" {
				t.Errorf("captured kind: got %v, want credit_memo", v["kind"])
			}
			var want any
			if c.reference {
				want = id(invoice)
			}
			if v["reference_invoice_id"] != want {
				t.Errorf("captured reference: got %v, want %v", v["reference_invoice_id"], want)
			}
			if got := v["gross_amount"].(map[string]any)["amount"]; got != "28.56" {
				t.Errorf("captured gross: got %v, want 28.56", got)
			}
			item := v["items"].([]any)[0].(map[string]any)
			if got := item["amount"].(map[string]any)["amount"]; got != "24.00" {
				t.Errorf("captured item amount: got %v, want 24.00", got)
			}
			if got := item["quantity"].(map[string]any)["value"]; got != "2" {
				t.Errorf("captured item quantity: got %v, want 2", got)
			}
			if v["status"] != c.status {
				t.Errorf("captured status: got %v, want %s", v["status"], c.status)
			}
			if got, _ := v["hold_reason"].(string); got != c.reason {
				t.Errorf("captured hold reason: got %q, want %q", got, c.reason)
			}
			if m := e.act(t, 200, id(v), "match", ""); m["is_clean"] != true {
				t.Errorf("match of the captured memo: got issues %v, want clean", m["issues"])
			}
		})
	}
	refused := map[string]*capture.Extraction{
		"memo of both signs":         with(memo("CM-5", "-"), func(x *capture.Extraction) { x.TaxAmount = "4.56" }),
		"invoice naming an invoice":  with(extraction(), func(x *capture.Extraction) { x.InvoiceNumber, x.CreditedInvoiceNumber = "CAP-9", "INV-1" }),
		"invoice of negative amount": with(extraction(), func(x *capture.Extraction) { x.InvoiceNumber, x.Lines[0].Amount = "CAP-8", "-120" }),
	}
	for name, x := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := run(x, "%PDF-1 "+name)
			pgtest.WantErr(t, err, "payables.unreadable_document")
		})
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "capture a credit memo", func(ctx context.Context) error {
		_, err := endpoints(extractor{e: memo("CM-F", "-")}).CaptureInvoice(ctx, e.DE, []byte("%PDF failing memo"))
		return err
	})
}
