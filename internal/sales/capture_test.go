package sales_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// reader is a model that reads every document as ext, or fails with err;
// it keeps what it was given.
type reader struct {
	ext         capture.OrderExtraction
	err         error
	contentType *string
}

func (r reader) ExtractOrder(_ context.Context, _ []byte, contentType string) (*capture.OrderExtraction, error) {
	if r.contentType != nil {
		*r.contentType = contentType
	}
	if r.err != nil {
		return nil, r.err
	}
	ext := r.ext
	return &ext, nil
}

// purchaseOrder is Northwind's order as the model reads it: 250 brackets
// by their item key without a price, and an hour of set-up at 120.00.
func purchaseOrder() capture.OrderExtraction {
	return capture.OrderExtraction{CustomerName: "Northwind Traders", CustomerNumber: "BP70000001", OrderNumber: "NW-4711", Currency: "EUR",
		Items: []capture.OrderItem{{ItemKey: "BRACKET-M8", Description: "Machined bracket M8", Quantity: "250", Unit: "EA"},
			{Description: "Set-up", Quantity: "1", Unit: "H", UnitPrice: "120.00"}}, Confidence: "high"}
}

// capturing is a tenant as setup's, whose API reads orders with the
// reader and keeps uploads in a directory, with the permissions to
// upload and capture, and a price of the brackets in EUR.
func capturing(t *testing.T, r capture.OrderExtractor) env {
	t.Helper()
	e := setup(t)
	for _, perm := range []string{"sales.order.capture", "payables.invoice.capture", "platform.operation.read"} {
		e.Tenant.Grant(t, perm, e.Tenant.Human, e.Tenant.Agent, e.Tenant.Service)
	}
	store := blobstore.Dir{Root: t.TempDir()}
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Objects, cfg.OrderExtractor = store, r })
	e.Must(201, "POST", "/v1/sales-prices", fmt.Sprintf(`{"legal_entity_id": %q, "item_key": "BRACKET-M8", "valid_from": "2020-01-01",
		"unit_price": {"amount": "11.00", "currency": "EUR"}, "price_unit": 1, "unit": "EA"}`, e.DE), "")
	return e
}

// uploaded uploads the document of the content type for order capture and
// returns the upload's id.
func (e env) uploaded(t *testing.T, document, contentType string) string {
	t.Helper()
	up, _ := e.Must(201, "POST", "/v1/sales-order-uploads", fmt.Sprintf(`{"content_type": %q}`, contentType), "")
	r := e.API.Do(apitest.Req{Method: "PUT", Path: up["url"].(string), Body: document, Header: map[string][]string{"Content-Type": {contentType}}})
	if r.Code != 204 {
		t.Fatalf("upload: got %d %s, want 204", r.Code, r.Body)
	}
	return up["id"].(string)
}

// captureBody is the JSON of a capture of the upload into company 2000 on
// the revenue account, with further fields led by a comma.
func (e env) captureBody(upload, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "upload_id": %q, "account_id": %q%s}`, e.DE, upload, e.revenue, extra)
}

// captured captures the upload and returns the order its operation names.
func (e env) captured(t *testing.T, upload string) map[string]any {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/sales-orders:capture", e.captureBody(upload, `, "tax_code": "A1"`), "")
	if op["status"] != "succeeded" {
		t.Fatalf("operation: got %v, want succeeded", op)
	}
	order, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	return order
}

// TestCaptureOrder (T90a; ADR-0067 As built T90a): a purchase order
// uploaded as plain text is read into a draft order of the customer whose
// number it prints, an item without a price at its key's price record and
// one with a price at the price printed, on the request's account and tax
// code; the order keeps what was read, the upload names it, and the same
// upload captured again answers the same order.
func TestCaptureOrder(t *testing.T) {
	var contentType string
	e := capturing(t, reader{ext: purchaseOrder(), contentType: &contentType})
	upload := e.uploaded(t, "Purchase order NW-4711 of Northwind Traders, customer BP70000001", "text/plain")
	order := e.captured(t, upload)
	fields(t, order, map[string]any{"status": "draft", "customer_id": e.customer.String(), "currency": "EUR", "credit_block": "none"})
	if contentType != "text/plain" {
		t.Errorf("document read as: got %q, want text/plain", contentType)
	}
	var got []string
	for _, it := range items(t, order) {
		got = append(got, fmt.Sprintf("%v %v %s %s %v %s %v %v", it["item_no"], it["item_key"], quantity(t, it, "quantity"), amount(t, it, "unit_price"),
			it["price_source"], amount(t, it, "net_amount"), it["account_id"] == e.revenue.String(), it["tax_code"]))
	}
	if want := "1 BRACKET-M8 250 EA 11.00 EUR price_list 2750.00 EUR true A1, 2 <nil> 1 H 120.00 EUR manual 120.00 EUR true A1"; strings.Join(got, ", ") != want {
		t.Errorf("items: got %s, want %s", strings.Join(got, ", "), want)
	}
	c, _ := order["capture"].(map[string]any)
	fields(t, c, map[string]any{"upload_id": upload, "customer_reference": "NW-4711", "confidence": "high"})
	if got := fmt.Sprint(c["customer_matched_by"]); got != "[customer_number]" {
		t.Errorf("customer matched by: got %s, want [customer_number]", got)
	}
	var read []string
	for _, it := range items(t, c) {
		read = append(read, fmt.Sprintf("%v %q %v %v %q %v", it["item_no"], it["item_key"], it["quantity"], it["unit"], it["unit_price"], it["price_source"]))
	}
	if want := `1 "BRACKET-M8" 250 EA "" price_list, 2 "" 1 H "120.00" manual`; strings.Join(read, ", ") != want {
		t.Errorf("capture's items: got %s, want %s", strings.Join(read, ", "), want)
	}
	up, _ := e.Must(200, "GET", "/v1/sales-order-uploads/"+upload, "", "")
	fields(t, up, map[string]any{"status": "used", "used_by": order["id"]})
	again := e.captured(t, upload)
	if again["id"] != order["id"] {
		t.Errorf("the same upload captured again: got order %v, want %v", again["id"], order["id"])
	}
	other := e.Problem(422, "payables.upload_used", "POST", "/v1/sales-orders:capture",
		fmt.Sprintf(`{"legal_entity_id": %q, "upload_id": %q, "account_id": %q}`, e.US, upload, e.revenue), "")
	if strings.Contains(fmt.Sprint(other), order["id"].(string)) {
		t.Errorf("the same upload into another company: got %v, want no order named", other)
	}
	if n := e.count(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1`); n != 1 {
		t.Errorf("orders: got %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'sales.sales_order.create' AND object_id = $2
		AND detail->>'capture_upload_id' = $3`, order["id"], upload); n != 1 {
		t.Errorf("audit records naming the upload: got %d, want 1", n)
	}
	confirmed, _ := e.Must(200, "POST", "/v1/sales-orders/"+order["id"].(string)+":confirm", "", `"1"`)
	fields(t, confirmed, map[string]any{"status": "confirmed"})
}

// count is the number the query gives for the tenant, whose id is $1.
func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	pgtest.Scan(t, query, append([]any{e.Tenant.ID}, args...), &n)
	return n
}

// TestCaptureOrderByTaxNumber (T90a): a document that prints the
// customer's tax registration number and no customer number finds the
// customer by it, whatever its scheme; one without a currency is in the
// company's, and a confidence below high is a warning.
func TestCaptureOrderByTaxNumber(t *testing.T) {
	ext := purchaseOrder()
	ext.CustomerNumber, ext.CustomerTaxNumber, ext.Currency, ext.Confidence = "", "DE811111111", "", "medium"
	e := capturing(t, reader{ext: ext})
	e.taxNumber(t, e.customer, "EU_VAT", "DE811111111")
	op, _ := e.Must(202, "POST", "/v1/sales-orders:capture", e.captureBody(e.uploaded(t, "%PDF-1.7 order", "application/pdf"), ""), "")
	result, _ := op["result"].(map[string]any)
	if got := fmt.Sprint(result["warnings"]); !strings.Contains(got, "confidence is medium") {
		t.Errorf("warnings: got %s, want the confidence named", got)
	}
	order, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	c, _ := order["capture"].(map[string]any)
	if got := fmt.Sprint(order["customer_id"], " ", c["customer_matched_by"], " ", order["currency"]); got != e.customer.String()+" [tax_number] EUR" {
		t.Errorf("customer, how and currency: got %s, want the customer by [tax_number] in the company's EUR", got)
	}
}

// taxNumber gives the partner a tax identifier of the scheme and value, as
// an approved partner change seals one.
func (e env) taxNumber(t *testing.T, partner uuid.UUID, scheme, value string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, $3, '\x00', $4, $5, $6, $6)`, e.Tenant.ID, partner, scheme, apitest.Fingerprint(t, e.Tenant.ID, value), value[len(value)-4:], e.Tenant.Human.ID)
}

// TestCaptureOrderRefusals (T90a): a customer the document does not name,
// or names twice, an item with neither a price nor a price record, a
// document without items, one the model does not read, a refusal of the
// model gateway, an account the company cannot post and a server without
// a model are refused, each creating nothing.
func TestCaptureOrderRefusals(t *testing.T) {
	ext := func(change func(*capture.OrderExtraction)) capture.OrderExtraction {
		x := purchaseOrder()
		change(&x)
		return x
	}
	budget := problem.New(429, "agents.budget_exhausted", "Too many requests", "spent")
	cases := []struct {
		name         string
		r            capture.OrderExtractor
		body         func(e env, upload string) string
		status       int
		code, field  string
		otherPartner bool
	}{
		{"an unknown customer", reader{ext: ext(func(x *capture.OrderExtraction) { x.CustomerNumber = "BP99" })}, nil, 422, "sales.capture_customer_unknown", "", false},
		{"two customers", reader{ext: ext(func(x *capture.OrderExtraction) { x.CustomerTaxNumber = "DE822222222" })}, nil, 422,
			"sales.capture_customer_ambiguous", "", true},
		{"an item without a price", reader{ext: ext(func(x *capture.OrderExtraction) { x.Items[0].ItemKey = "" })}, nil, 422, "sales.no_price",
			"items[0].unit_price", false},
		{"no items", reader{ext: ext(func(x *capture.OrderExtraction) { x.Items = nil })}, nil, 422, "sales.unreadable_document", "", false},
		{"an unreadable document", reader{err: errors.New("extraction is not valid JSON")}, nil, 422, "sales.unreadable_document", "", false},
		{"the gateway's refusal", reader{err: fmt.Errorf("capture: %w", budget)}, nil, 429, "agents.budget_exhausted", "", false},
		{"an account not postable", reader{ext: purchaseOrder()}, func(e env, upload string) string {
			return fmt.Sprintf(`{"legal_entity_id": %q, "upload_id": %q, "account_id": %q}`, e.DE, upload, uuid.New())
		}, 422, "sales.account_not_postable", "account_id", false},
		{"an unknown tax code", reader{ext: purchaseOrder()}, func(e env, upload string) string { return e.captureBody(upload, `, "tax_code": "ZZ"`) },
			422, "sales.unknown_tax_code", "tax_code", false},
		{"a code levying its lines' subdivision without a jurisdiction", reader{ext: purchaseOrder()},
			func(e env, upload string) string { return e.captureBody(upload, `, "tax_code": "AJ"`) }, 422, "sales.tax_jurisdiction_required", "tax_jurisdiction", false},
		{"a jurisdiction its code does not levy", reader{ext: purchaseOrder()},
			func(e env, upload string) string {
				return e.captureBody(upload, `, "tax_code": "A1", "tax_jurisdiction": "DE-BY"`)
			},
			422, "sales.tax_jurisdiction_not_levied", "tax_jurisdiction", false},
		{"without a model", nil, nil, 503, "platform.unavailable", "", false},
		{"an unknown upload", reader{ext: purchaseOrder()}, func(e env, _ string) string { return e.captureBody(uuid.NewString(), "") },
			404, "request.not_found", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := capturing(t, c.r)
			if c.otherPartner {
				other := e.partner(t, "BP70000002", "Northwind Holdings", "active", "customer")
				e.taxNumber(t, other, "EU_VAT", "DE822222222")
			}
			upload := e.uploaded(t, "an order", "text/plain")
			body := e.captureBody(upload, "")
			if c.body != nil {
				body = c.body(e, upload)
			}
			got := e.Problem(c.status, c.code, "POST", "/v1/sales-orders:capture", body, "")
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
			if detail, _ := got["detail"].(string); strings.Contains(detail, "valid JSON") { // a type assertion that fails leaves "", which passes
				t.Errorf("detail: got %q, want the reader's own words kept out of it", detail)
			}
			if n := e.count(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1`); n != 0 {
				t.Errorf("orders: got %d, want none", n)
			}
			up, _ := e.Must(200, "GET", "/v1/sales-order-uploads/"+upload, "", "")
			fields(t, up, map[string]any{"status": "received"})
		})
	}
}

// TestCaptureOrderClosedToAgents (T90a): only a person captures an order;
// an upload for an invoice is not an order's.
func TestCaptureOrderClosedToAgents(t *testing.T) {
	e := capturing(t, reader{ext: purchaseOrder()})
	upload := e.uploaded(t, "an order", "text/plain")
	for _, a := range []pgtest.Actor{e.Tenant.Agent, e.Tenant.Service} {
		e.As(a).In(t).Problem(403, "identity.agent_not_allowed", "POST", "/v1/sales-orders:capture", e.captureBody(upload, ""), "")
	}
	invoice, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose": "supplier_invoice", "content_type": "application/pdf"}`, "")
	e.Problem(422, "payables.upload_purpose", "POST", "/v1/sales-orders:capture", e.captureBody(invoice["id"].(string), ""), "")
	e.Problem(422, "request.reference_not_found", "POST", "/v1/sales-orders:capture",
		fmt.Sprintf(`{"legal_entity_id": %q, "upload_id": %q, "account_id": %q}`, uuid.New(), upload, e.revenue), "")
	e.Problem(422, "payables.unsupported_content_type", "POST", "/v1/sales-order-uploads", `{"content_type": "image/png"}`, "")
}

// TestUploadPermissionFollowsPurpose (T90a, coordinator 2026-11-15, least
// privilege): a customer's purchase order is uploaded and read under the
// order capture's permission and a supplier invoice under the invoice
// capture's; neither one stands in for the other, and each upload is read
// only where its purpose is.
func TestUploadPermissionFollowsPurpose(t *testing.T) {
	e := capturing(t, reader{ext: purchaseOrder()})
	invoice := `{"purpose": "supplier_invoice", "content_type": "application/pdf"}`
	inv, _ := e.Must(201, "POST", "/v1/uploads", invoice, "")
	order, _ := e.Must(201, "POST", "/v1/sales-order-uploads", `{"content_type": "text/plain"}`, "")
	invoiceUpload, orderUpload := "/v1/uploads/"+inv["id"].(string), "/v1/sales-order-uploads/"+order["id"].(string)
	t.Run("each read where its purpose is", func(t *testing.T) {
		e := e.In(t)
		e.Problem(404, "request.not_found", "GET", "/v1/uploads/"+order["id"].(string), "", "")
		e.Problem(404, "request.not_found", "GET", "/v1/sales-order-uploads/"+inv["id"].(string), "", "")
	})
	t.Run("without the invoice capture's permission", func(t *testing.T) {
		e := e.In(t)
		e.Tenant.Revoke(t, "payables.invoice.capture", e.Tenant.Human)
		defer e.Tenant.Grant(t, "payables.invoice.capture", e.Tenant.Human)
		e.Problem(403, "identity.permission_missing", "POST", "/v1/uploads", invoice, "")
		e.Problem(403, "identity.permission_missing", "GET", invoiceUpload, "", "")
		e.Must(201, "POST", "/v1/sales-order-uploads", `{"content_type": "text/plain"}`, "")
		e.Must(200, "GET", orderUpload, "", "")
	})
	t.Run("without the order capture's permission", func(t *testing.T) {
		e := e.In(t)
		e.Tenant.Revoke(t, "sales.order.capture", e.Tenant.Human)
		defer e.Tenant.Grant(t, "sales.order.capture", e.Tenant.Human)
		e.Problem(403, "identity.permission_missing", "POST", "/v1/sales-order-uploads", `{"content_type": "text/plain"}`, "")
		e.Problem(403, "identity.permission_missing", "GET", orderUpload, "", "")
		e.Must(201, "POST", "/v1/uploads", invoice, "")
		e.Must(200, "GET", invoiceUpload, "", "")
	})
}

// TestCaptureOrderFailures calls the capture outside a unit of work and
// with each of its database statements failing in turn.
func TestCaptureOrderFailures(t *testing.T) {
	e := capturing(t, reader{ext: purchaseOrder()})
	upload := uuid.MustParse(e.uploaded(t, "an order", "text/plain"))
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	byBoth := purchaseOrder()
	byBoth.CustomerTaxNumber = "DE811111111"
	e.taxNumber(t, e.customer, "EU_VAT", "DE811111111")
	h := app.Sales(app.Config{Pages: pages, Now: time.Now, FieldKeys: apitest.FieldKeys(), OrderExtractor: reader{ext: byBoth}})
	h.Uploads = stubUploads{}
	call := func(ctx context.Context) error {
		_, err := h.CaptureSalesOrder(ctx, &gen.SalesOrderCaptureRequest{LegalEntityID: e.DE, UploadID: upload, AccountID: e.revenue}, gen.CaptureSalesOrderParams{})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "capture", call)
	t.Run("the upload's refusal", func(t *testing.T) {
		h.Uploads = stubUploads{err: problem.Unprocessable("payables.upload_empty", "empty")}
		if p := problem.From(e.inRequest(t, call)); p.Code != "payables.upload_empty" {
			t.Errorf("got %v, want payables.upload_empty", p)
		}
	})
	t.Run("closing the upload", func(t *testing.T) {
		h.Uploads = stubUploads{usedErr: errors.New("upload closed")}
		if err := e.inRequest(t, call); err == nil || !strings.Contains(err.Error(), "upload closed") {
			t.Errorf("got %v, want the upload's error", err)
		}
	})
}

// stubUploads hands out a document and closes nothing, or fails.
type stubUploads struct{ err, usedErr error }

func (s stubUploads) Take(context.Context, uuid.UUID, string) ([]byte, string, error) {
	return []byte("an order"), "text/plain", s.err
}

func (s stubUploads) Used(context.Context, uuid.UUID, []byte, uuid.UUID) error { return s.usedErr }

var _ sales.Uploads = stubUploads{}
