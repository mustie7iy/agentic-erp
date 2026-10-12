package payables_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// extractor returns a fixed extraction, or fails.
type extractor struct {
	e   *capture.Extraction
	err error
}

func (x extractor) Extract(context.Context, []byte) (*capture.Extraction, error) { return x.e, x.err }

// orderNumbers looks orders up by number the way procurement will.
type orderNumbers struct{}

func (orderNumbers) OrderByNumber(ctx context.Context, le uuid.UUID, number string) (uuid.UUID, bool, error) {
	tx, _, _ := reqctx.Request(ctx)
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM procurement.purchase_order WHERE legal_entity_id = $1 AND document_number = $2`, le, number).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// endpoints is the payables wiring of the API, with a capture extractor.
func endpoints(x capture.Extractor) *payables.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &payables.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Suppliers: partner.Suppliers{}, Orders: procurement.Orders{},
		Postings: ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}},
		Accounts: ledger.Accounts{}, Numbers: configuration.Numbers{}, Calendar: ledger.Calendar{LegalEntities: tenancy.Directory{}},
		Settings: configuration.Settings{}, Approvals: fixture.Approvals(), Payments: payments.Reservations{}, OrderNumbers: orderNumbers{}, Extractor: x}
}

func extraction() *capture.Extraction {
	return &capture.Extraction{VendorName: "Supplier", InvoiceNumber: "CAP-1", InvoiceDate: "2026-03-10", PurchaseOrder: "PO-1", Currency: "EUR",
		Lines:     []capture.Line{{Description: "Paper", Quantity: "10", Amount: "120"}, {Description: "Toner", Quantity: "5", Amount: "15.00"}},
		TaxAmount: "22.8", GrossAmount: "157.80", Confidence: "high"}
}

func TestCapture(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	run := func(x capture.Extractor, doc string) (gen.CaptureSupplierInvoiceRes, error) {
		var res gen.CaptureSupplierInvoiceRes
		err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			res, err = endpoints(x).CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString(doc)},
				gen.CaptureSupplierInvoiceParams{})
			return err
		})
		return res, err
	}
	res, err := run(extractor{e: extraction()}, "%PDF-1 invoice one")
	if err != nil {
		t.Fatal(err)
	}
	op := res.(*gen.OperationHeaders).Response
	if op.Status != "succeeded" || op.Kind != "payables.capture" || op.Resource.Null {
		t.Fatalf("operation: %+v", op)
	}
	inv, _ := e.Must(200, "GET", op.Resource.Value, "", "")
	if inv["source"] != "capture" || inv["status"] != "parked" || inv["supplier_id"] != e.Supplier.String() || len(inv["items"].([]any)) != 2 ||
		inv["tax_amount"].(map[string]any)["amount"] != "22.80" {
		t.Fatalf("captured: %v", inv)
	}
	if ev := events(t, e.Tenant, "supplier_invoice_captured"); len(ev) != 1 || len(ev[0]["capture_sha256"].(string)) != 64 {
		t.Fatalf("events: %v", ev)
	}
	if _, err := run(extractor{e: extraction()}, "%PDF-1 invoice one"); err == nil || !strings.Contains(err.Error(), "payables.already_captured") {
		t.Fatalf("captured twice: %v", err)
	}
	// Low confidence: on hold, with lines numbered by the order.
	low := extraction()
	low.InvoiceNumber, low.Confidence = "CAP-2", "low"
	low.Lines = []capture.Line{{Description: "Toner", Quantity: "5", Amount: "15.00", POLine: 2}}
	low.GrossAmount, low.TaxAmount = "15.00", "0"
	res, err = run(extractor{e: low}, "%PDF-1 invoice two")
	if err != nil {
		t.Fatal(err)
	}
	held, _ := e.Must(200, "GET", res.(*gen.OperationHeaders).Response.Resource.Value, "", "")
	if held["status"] != "on_hold" || len(held["items"].([]any)) != 1 {
		t.Fatalf("low confidence: %v", held)
	}

	refusals := map[string]func(*capture.Extraction){
		"no order":       func(x *capture.Extraction) { x.PurchaseOrder = "" },
		"unknown order":  func(x *capture.Extraction) { x.PurchaseOrder = "PO-9" },
		"date":           func(x *capture.Extraction) { x.InvoiceDate = "10.03.2026" },
		"number":         func(x *capture.Extraction) { x.InvoiceNumber = "" },
		"currency":       func(x *capture.Extraction) { x.Currency = "USD" },
		"gross":          func(x *capture.Extraction) { x.GrossAmount = "lots" },
		"tax":            func(x *capture.Extraction) { x.TaxAmount = "1.234" },
		"no lines":       func(x *capture.Extraction) { x.Lines = nil },
		"line amount":    func(x *capture.Extraction) { x.Lines[0].Amount = "x" },
		"unknown line":   func(x *capture.Extraction) { x.Lines = x.Lines[:1] },
		"validation":     func(x *capture.Extraction) { x.Lines[0].Quantity = "-1" },
		"extraction err": nil,
	}
	for name, change := range refusals {
		x := extractor{e: extraction()}
		if change == nil {
			x = extractor{err: errors.New("model unavailable")}
		} else {
			change(x.e)
		}
		if _, err := run(x, "%PDF-1 "+name); err == nil {
			t.Errorf("%s: captured", name)
		}
	}
	if _, err := run(nil, "%PDF"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("no extractor: %v", err)
	}
	if _, err := run(extractor{e: extraction()}, strings.Repeat("x", payables.MaxDocument+1)); err == nil {
		t.Error("oversized document captured")
	}
	if _, err := endpoints(nil).CaptureInvoice(context.Background(), e.DE, nil); err == nil {
		t.Error("capture outside a unit of work")
	}
	// Captures are served by the API only once an extractor is configured.
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}},
		Body:   "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() + "\r\n--b\r\nContent-Disposition: form-data; name=\"document\"\r\n\r\n%PDF\r\n--b--\r\n"})
	if r.Code != 503 {
		t.Fatalf("capture through the API: %d %s", r.Code, r.Body)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "capture", func(ctx context.Context) error {
		x := extraction()
		x.InvoiceNumber = "CAP-F"
		_, err := endpoints(extractor{e: x}).CaptureInvoice(ctx, e.DE, []byte("%PDF failing"))
		return err
	})
}
