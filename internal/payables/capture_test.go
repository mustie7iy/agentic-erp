package payables_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM procurement.purchase_order WHERE legal_entity_id = $1 AND document_number = $2`, le, number).Scan(&id)
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
		Settings: configuration.Settings{}, Schemas: configuration.Settings{}, References: partner.References{}, Rates: rates, Approvals: fixture.Approvals(), Payments: payments.Reservations{}, OrderNumbers: orderNumbers{}, Extractor: x}
}

func extraction() *capture.Extraction {
	return &capture.Extraction{DocumentKind: "invoice", VendorName: "Supplier", InvoiceNumber: "CAP-1", InvoiceDate: "2026-03-10", PurchaseOrder: "PO-1", Currency: "EUR",
		Lines:     []capture.Line{{Description: "Paper", Quantity: "10", Amount: "120"}, {Description: "Toner", Quantity: "5", Amount: "15.00"}},
		TaxAmount: "22.8", GrossAmount: "157.80", Confidence: "high"}
}

// A document carries no custom fields: one the tenant's schema requires
// holds the captured invoice, naming the field, instead of refusing it,
// and the invoice's next update requires it.
func TestCaptureMissingRequiredField(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	pgtest.Unguarded(t, `UPDATE config.config_version SET document = jsonb_set(document, '{extensions,supplier_invoice,required}', '["cost_center"]')
		WHERE tenant_id = $1`, e.Tenant.ID)
	var res gen.CaptureSupplierInvoiceRes
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		res, err = endpoints(extractor{e: extraction()}).CaptureSupplierInvoice(ctx,
			&gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE, Document: gen.NewOptString("%PDF-1 needs a cost center")}, gen.CaptureSupplierInvoiceParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := res.(*gen.OperationHeaders).Response.Resource.Value
	held, tag := e.Must(200, "GET", path, "", "")
	if held["status"] != "on_hold" {
		t.Errorf("status: got %v, want on_hold", held["status"])
	}
	if r, _ := held["hold_reason"].(string); !strings.Contains(r, "cost_center") {
		t.Errorf("hold reason: got %q, want it to name cost_center", r)
	}
	var schema *int
	pgtest.Scan(t, `SELECT extensions_schema FROM payables.supplier_invoice WHERE id = $1`, []any{path[len("/v1/supplier-invoices/"):]}, &schema)
	if schema == nil || *schema != 1 {
		t.Errorf("extensions schema: got %v, want 1", schema)
	}
	released, tag := e.Must(200, "POST", path+":release", `{"comment": "checked"}`, tag)
	e.Problem(422, "request.invalid_extension", "POST", path+":post", "", tag)
	e.Problem(422, "request.invalid_extension", "PATCH", path, `{"extensions": {"channel": "mail"}}`, tag)
	if released["status"] != "parked" {
		t.Errorf("released status: got %v, want parked", released["status"])
	}
	e.Must(200, "PATCH", path, `{"extensions": {"cost_center": "CC1"}}`, tag)
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
	if op.Status != "succeeded" {
		t.Errorf("operation status: got %q, want %q", op.Status, "succeeded")
	}
	if op.Kind != "payables.capture" {
		t.Errorf("operation kind: got %q, want %q", op.Kind, "payables.capture")
	}
	if op.Resource.Null {
		t.Fatalf("operation resource: got null, want the invoice: %+v", op)
	}
	inv, _ := e.Must(200, "GET", op.Resource.Value, "", "")
	if inv["source"] != "capture" {
		t.Errorf("captured source: got %v, want capture", inv["source"])
	}
	if inv["status"] != "parked" {
		t.Errorf("captured status: got %v, want parked", inv["status"])
	}
	if inv["supplier_id"] != e.Supplier.String() {
		t.Errorf("captured supplier: got %v, want %v", inv["supplier_id"], e.Supplier)
	}
	if got := len(inv["items"].([]any)); got != 2 {
		t.Errorf("captured items: got %d, want 2", got)
	}
	if got := inv["tax_amount"].(map[string]any)["amount"]; got != "22.80" {
		t.Errorf("captured tax amount: got %v, want 22.80", got)
	}
	ev := events(t, e.Tenant, "supplier_invoice_captured")
	if len(ev) != 1 {
		t.Fatalf("captured events: got %d, want 1: %v", len(ev), ev)
	}
	if got := len(ev[0]["capture_sha256"].(string)); got != 64 {
		t.Errorf("capture hash length: got %d, want 64", got)
	}
	_, err = run(extractor{e: extraction()}, "%PDF-1 invoice one")
	pgtest.WantErr(t, err, "payables.already_captured")
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
	if held["status"] != "on_hold" {
		t.Errorf("low confidence status: got %v, want on_hold", held["status"])
	}
	if got := len(held["items"].([]any)); got != 1 {
		t.Errorf("low confidence items: got %d, want 1", got)
	}

	// Each refusal is of a document not captured before, so it is refused
	// for its own reason rather than as a duplicate.
	unreadable := "payables.unreadable_document"
	refusals := map[string]struct {
		change func(*capture.Extraction)
		code   string
	}{
		"no order":       {func(x *capture.Extraction) { x.PurchaseOrder = "" }, "payables.capture_needs_order"},
		"unknown order":  {func(x *capture.Extraction) { x.PurchaseOrder = "PO-9" }, unreadable},
		"date":           {func(x *capture.Extraction) { x.InvoiceDate = "10.03.2026" }, unreadable},
		"number":         {func(x *capture.Extraction) { x.InvoiceNumber = "" }, unreadable},
		"currency":       {func(x *capture.Extraction) { x.Currency = "USD" }, unreadable},
		"gross":          {func(x *capture.Extraction) { x.GrossAmount = "lots" }, unreadable},
		"tax":            {func(x *capture.Extraction) { x.TaxAmount = "1.234" }, unreadable},
		"no lines":       {func(x *capture.Extraction) { x.Lines = nil }, unreadable},
		"line amount":    {func(x *capture.Extraction) { x.Lines[0].Amount = "x" }, unreadable},
		"unknown line":   {func(x *capture.Extraction) { x.Lines = x.Lines[:1] }, unreadable},
		"validation":     {func(x *capture.Extraction) { x.Lines[0].Quantity = "-1" }, "payables.invalid_quantity"},
		"document kind":  {func(x *capture.Extraction) { x.DocumentKind = "statement" }, unreadable},
		"negative":       {func(x *capture.Extraction) { x.GrossAmount = "-157.80" }, unreadable},
		"extraction err": {nil, unreadable},
	}
	for name, r := range refusals {
		t.Run(name, func(t *testing.T) {
			x := extractor{e: extraction()}
			x.e.InvoiceNumber = "CAP-" + name
			if r.change == nil {
				x = extractor{err: errors.New("model unavailable")}
			} else {
				r.change(x.e)
			}
			_, err := run(x, "%PDF-1 "+name)
			pgtest.WantErr(t, err, r.code)
		})
	}
	// The model gateway's own refusals reach the caller as they are: the
	// tenant's AI off, its budget spent, documents withheld (ADR-0036).
	for _, c := range []struct {
		refused *problem.Problem
		want    string
	}{
		{problem.Unprocessable("agents.documents_withheld", "Withheld."), "agents.documents_withheld"},
		{problem.Forbidden("agents.ai_disabled", "Off."), "agents.ai_disabled"},
		{problem.New(429, "agents.budget_exhausted", "Spent", "Spent."), "agents.budget_exhausted"},
		{problem.New(502, "agents.model_unreachable", "Unreachable", "Unreachable."), unreadable},
		{problem.BadRequest("request.invalid_body", "The gateway's own bug."), unreadable},
	} {
		code, refused := c.want, c.refused
		t.Run(refused.Code, func(t *testing.T) {
			x := extractor{err: fmt.Errorf("capture: extract invoice: %w", &url.Error{Op: "Post", URL: "http://model-gateway.internal", Err: refused})}
			_, err := run(x, "%PDF-1 "+refused.Code)
			if got := problem.From(err).Code; got != code {
				t.Errorf("code: got %s, want %s", got, code)
			}
		})
	}
	_, err = run(nil, "%PDF")
	pgtest.CheckErr(t, err, "not configured")
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
