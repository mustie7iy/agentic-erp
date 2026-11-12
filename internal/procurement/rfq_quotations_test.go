package procurement_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Supplier quotations (T82b, ADR-0069 §7). The RFQ of these tests asks for
// 40 chairs (item 1) and 12.5 desks (item 2) in EUR and invites the
// fixture's two suppliers.

// publishedRFQ is that RFQ, published; it returns its id.
func (e env) publishedRFQ(t *testing.T, due string) string {
	t.Helper()
	r := e.rfq(t, due)
	e.act(t, "/v1/rfqs/"+id(r), "publish", "")
	return id(r)
}

// price is a quoted price of an RFQ item.
func price(item int, amount, currency, extra string) string {
	return fmt.Sprintf(`{"rfq_item_no": %d, "unit_price": {"amount": %q, "currency": %q}%s}`, item, amount, currency, extra)
}

// quotationBody is a supplier's quotation at the prices.
func quotationBody(supplier uuid.UUID, reference string, prices ...string) string {
	return fmt.Sprintf(`{"supplier_id": %q, "supplier_reference": %q, "valid_until": "2026-12-31", "items": %s}`, supplier, reference, items(prices...))
}

func amountOf(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	money := apitest.Object(t, m, field)
	return fmt.Sprintf("%v %v", money["amount"], money["currency"])
}

func TestSupplierQuotations(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	rfq := e.publishedRFQ(t, "2026-07-31")
	path := "/v1/rfqs/" + rfq + "/quotations"
	// 40 × 412.50 = 16,500.00; 12.5 × 1,000.00 ÷ 3 = 4,166.666…, rounded half away from zero to 4,166.67.
	full, tag := e.Must(201, "POST", path, quotationBody(e.Supplier, "Q-457", price(2, "1000.00", "EUR", `, "price_unit": 3`), price(1, "412.50", "EUR", "")), "")
	t.Run("a complete quotation", func(t *testing.T) {
		for field, want := range map[string]any{"rfq_id": rfq, "rfq_document_number": "RQ100000", "supplier_id": e.Supplier.String(),
			"supplier_number": "BP90000001", "supplier_name": tyrell, "supplier_reference": "Q-457", "valid_until": "2026-12-31",
			"status": "recorded", "complete": true, "version": float64(1)} {
			if full[field] != want {
				t.Errorf("%s: got %v, want %v", field, full[field], want)
			}
		}
		if tag != `"1"` {
			t.Errorf("etag: got %s, want \"1\"", tag)
		}
		if got := apitest.List(t, full, "unquoted_item_nos"); len(got) != 0 {
			t.Errorf("unquoted_item_nos: got %v, want none", got)
		}
		if got := amountOf(t, full, "total"); got != "20666.67 EUR" {
			t.Errorf("total: got %s, want 20666.67 EUR", got)
		}
		var lines []string
		for _, it := range apitest.List(t, full, "items") {
			m := it.(map[string]any)
			q := apitest.Object(t, m, "quantity")
			lines = append(lines, fmt.Sprintf("%v %v %v %v at %s per %v = %s", m["rfq_item_no"], m["material_key"], q["value"], q["unit"],
				amountOf(t, m, "unit_price"), m["price_unit"], amountOf(t, m, "amount")))
		}
		want := "1 CHAIR 40 EA at 412.50 EUR per 1 = 16500.00 EUR; 2 DESK 12.5 EA at 1000.00 EUR per 3 = 4166.67 EUR"
		if got := strings.Join(lines, "; "); got != want {
			t.Errorf("items: got %s, want %s", got, want)
		}
	})
	part, _ := e.Must(201, "POST", path, quotationBody(e.Vendor, "ANG-9", price(1, "400.00", "EUR", "")), "")
	t.Run("a quotation that leaves an item out", func(t *testing.T) {
		if part["complete"] != false {
			t.Errorf("complete: got %v, want false", part["complete"])
		}
		if got := apitest.List(t, part, "unquoted_item_nos"); len(got) != 1 || got[0] != float64(2) {
			t.Errorf("unquoted_item_nos: got %v, want [2]", got)
		}
		if got := amountOf(t, part, "total"); got != "16000.00 EUR" {
			t.Errorf("total: got %s, want 16000.00 EUR", got)
		}
	})
	list := func(t *testing.T, q string) ([]string, string) {
		t.Helper()
		page, _ := e.in(t).Must(200, "GET", path+"?"+q, "", "")
		var out []string
		for _, it := range apitest.List(t, page, "items") {
			m := it.(map[string]any)
			if m["supplier_id"] == e.Vendor.String() && m["supplier_name"] != cabot {
				t.Errorf("quotation %v: got supplier_name %v, want %s", m["supplier_reference"], m["supplier_name"], cabot)
			}
			out = append(out, fmt.Sprintf("%v %v %s", m["supplier_number"], m["supplier_reference"], amountOf(t, m, "total")))
		}
		tok, _ := page["next_page_token"].(string)
		return out, tok
	}
	t.Run("the RFQ's quotations", func(t *testing.T) {
		got, _ := list(t, "")
		if want := "BP90000001 Q-457 20666.67 EUR, BP90000002 ANG-9 16000.00 EUR"; strings.Join(got, ", ") != want {
			t.Errorf("as recorded: got %v, want %s", got, want)
		}
		got, _ = list(t, "order_by=created_at%20desc")
		if want := "BP90000002 ANG-9 16000.00 EUR, BP90000001 Q-457 20666.67 EUR"; strings.Join(got, ", ") != want {
			t.Errorf("newest first: got %v, want %s", got, want)
		}
		got, _ = list(t, "supplier_id="+e.Vendor.String())
		if want := "BP90000002 ANG-9 16000.00 EUR"; strings.Join(got, ", ") != want {
			t.Errorf("one supplier's: got %v, want %s", got, want)
		}
		first, tok := list(t, "page_size=1")
		if len(first) != 1 || tok == "" {
			t.Fatalf("page 1: got %v and token %q, want one quotation and a token", first, tok)
		}
		second, tok := list(t, "page_size=1&page_token="+tok)
		if len(second) != 1 || tok != "" || second[0] == first[0] {
			t.Errorf("page 2: got %v and token %q, want the other quotation and no token", second, tok)
		}
		e.in(t).Problem(400, "request.invalid_order_by", "GET", path+"?order_by=valid_until", "", "")
	})
	t.Run("events and audit", func(t *testing.T) {
		recorded := apitest.CompanyNamed(t, events(t, e, "supplier_quotation_recorded"))
		if len(recorded) != 2 {
			t.Fatalf("supplier_quotation_recorded events: got %d, want 2", len(recorded))
		}
		want := map[string]any{"supplier_quotation_id": id(full), "rfq_id": rfq, "document_number": "RQ100000", "legal_entity_id": e.DE.String(),
			"legal_entity_code": "2000",
			"supplier_id":       e.Supplier.String(), "supplier_number": "BP90000001", "supplier_reference": "Q-457", "valid_until": "2026-12-31",
			"total": map[string]any{"amount": "20666.67", "currency": "EUR"}, "complete": true, "item_count": float64(2)}
		if !reflect.DeepEqual(recorded[0], want) {
			t.Errorf("first event: got %v, want %v (the supplier by id and number, no name)", recorded[0], want)
		}
		if recorded[1]["complete"] != false || recorded[1]["supplier_number"] != "BP90000002" {
			t.Errorf("second event: got %v, want BP90000002's incomplete quotation", recorded[1])
		}
		// A supplier's name goes into no event (product owner 2026-11-05).
		for _, name := range []string{tyrell, cabot} {
			apitest.NeverPublished(t, e.Tenant, "procurement", name)
		}
		for _, q := range []map[string]any{full, part} {
			if got := audits(t, e, "procurement.supplier_quotation.record", id(q)); got != 1 {
				t.Errorf("audit records of quotation %s: got %d, want 1", q["supplier_reference"], got)
			}
		}
	})
}

// TestSupplierQuotationRefusals: what a quotation is refused for, each by
// the request's own field.
func TestSupplierQuotationRefusals(t *testing.T) {
	e := setupIn2026(t)
	rfq := e.publishedRFQ(t, "2026-07-31")
	path := "/v1/rfqs/" + rfq + "/quotations"
	draft := "/v1/rfqs/" + id(e.rfq(t, "2026-07-31")) + "/quotations"
	stranger := e.NewSupplier(t, "BP90000009")
	ok := price(1, "400.00", "EUR", "")
	e.Must(201, "POST", path, quotationBody(e.Vendor, "ANG-9", ok), "")
	for name, c := range map[string]struct {
		path, body string
		status     int
		code       string
		field      string
	}{
		"a draft RFQ":            {draft, quotationBody(e.Supplier, "Q", ok), 409, "procurement.invalid_status", ""},
		"a supplier not invited": {path, quotationBody(stranger, "Q", ok), 422, "procurement.supplier_not_invited", "supplier_id"},
		"a second quotation":     {path, quotationBody(e.Vendor, "ANG-10", ok), 409, "procurement.quotation_exists", ""},
		"another currency":       {path, quotationBody(e.Supplier, "Q", ok, price(2, "5.00", "USD", "")), 422, "procurement.currency_mismatch", "items[1].unit_price.currency"},
		"a price of zero":        {path, quotationBody(e.Supplier, "Q", price(1, "0.00", "EUR", "")), 422, "procurement.invalid_amount", "items[0].unit_price.amount"},
		"three decimals":         {path, quotationBody(e.Supplier, "Q", price(1, "1.005", "EUR", "")), 400, "request.invalid_amount", "items[0].unit_price.amount"},
		"no such RFQ item":       {path, quotationBody(e.Supplier, "Q", ok, price(3, "5.00", "EUR", "")), 422, "procurement.unknown_rfq_item", "items[1].rfq_item_no"},
		"an RFQ item twice":      {path, quotationBody(e.Supplier, "Q", ok, price(2, "5.00", "EUR", ""), price(1, "6.00", "EUR", "")), 422, "procurement.quotation_item_repeated", "items[2].rfq_item_no"},
		"a blank reference":      {path, quotationBody(e.Supplier, " ", ok), 422, "request.validation_failed", "supplier_reference"},
		"no price":               {path, quotationBody(e.Supplier, "Q"), 400, "request.invalid_body", ""},
		"a price unit of zero":   {path, quotationBody(e.Supplier, "Q", price(1, "5.00", "EUR", `, "price_unit": 0`)), 400, "request.invalid_body", ""},
		"a total out of range":   {path, quotationBody(e.Supplier, "Q", price(1, "90000000000000000.00", "EUR", "")), 422, "procurement.amount_too_large", "items"},
		"no RFQ with the id":     {"/v1/rfqs/" + uuid.NewString() + "/quotations", quotationBody(e.Supplier, "Q", ok), 404, "request.not_found", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(c.status, c.code, "POST", c.path, c.body, "")
			if c.field == "" {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
				t.Errorf("field errors: got %v, want one on %s", p["errors"], c.field)
			}
		})
	}
	t.Run("a caller of another company", func(t *testing.T) {
		other := e.As(e.Reader(t, "Reader of 1000", e.US)).In(t)
		other.Problem(404, "request.not_found", "POST", path, quotationBody(e.Supplier, "Q", ok), "")
		other.Problem(404, "request.not_found", "GET", path, "", "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.rfq.manage'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "POST", path, quotationBody(e.Supplier, "Q", ok), "")
		e.in(t).Must(200, "GET", path, "", "")
	})
	if n := len(events(t, e, "supplier_quotation_recorded")); n != 1 {
		t.Errorf("supplier_quotation_recorded events after the refusals: got %d, want 1", n)
	}
}

// TestSupplierQuotationReplayed: the same request with its Idempotency-Key
// again answers what it answered and records nothing more; the same
// quotation under a new key is the supplier's second and is refused. The
// key is kept by the wall clock, so the test runs on it, with dates no run
// reaches.
func TestSupplierQuotationReplayed(t *testing.T) {
	e := setup(t)
	path := "/v1/rfqs/" + e.publishedRFQ(t, "2999-12-31") + "/quotations"
	req := apitest.Req{Method: http.MethodPost, Path: path, Body: quotationBody(e.Supplier, "Q-457", price(1, "412.50", "EUR", "")),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, again := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || again.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice", first.Code, again.Code)
	}
	if again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", again.Header().Get("Idempotent-Replayed"))
	}
	if got, want := apitest.JSON(t, again), apitest.JSON(t, first); !reflect.DeepEqual(got, want) {
		t.Errorf("replayed body: got %v, want %v", got, want)
	}
	if want := path + "?supplier_id=" + e.Supplier.String(); first.Header().Get("Location") != want {
		t.Errorf("Location: got %q, want %q", first.Header().Get("Location"), want)
	}
	found, _ := e.Must(200, "GET", first.Header().Get("Location"), "", "")
	if got := apitest.List(t, found, "items"); len(got) != 1 || got[0].(map[string]any)["id"] != apitest.JSON(t, first)["id"] {
		t.Errorf("at its Location: got %v, want the quotation alone", got)
	}
	e.Problem(409, "procurement.quotation_exists", "POST", path, req.Body, "")
}

// TestSupplierQuotationOncePerSupplier: two quotations of one supplier
// recorded at once serialize on the RFQ's row lock; the second finds the
// first and is refused, and one is published.
func TestSupplierQuotationOncePerSupplier(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	rfq := uuid.MustParse(e.publishedRFQ(t, "2026-07-31"))
	record := func(ctx context.Context) error {
		_, err := h.CreateSupplierQuotation(ctx, &gen.SupplierQuotationCreate{SupplierID: e.Supplier, SupplierReference: "Q", ValidUntil: dateOf("2026-12-31"),
			Items: []gen.SupplierQuotationItemInput{{RfqItemNo: 1, UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"}}}}, gen.CreateSupplierQuotationParams{ID: rfq})
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, record, record)
	if first != nil {
		t.Fatalf("first quotation: %v", first)
	}
	if got := problem.From(second).Code; got != "procurement.quotation_exists" {
		t.Errorf("second quotation: got %s (%v), want procurement.quotation_exists", got, second)
	}
	if n := len(events(t, e, "supplier_quotation_recorded")); n != 1 {
		t.Errorf("supplier_quotation_recorded events: got %d, want 1", n)
	}
}

// TestSupplierQuotationDatabaseFailures calls the quotation handlers
// outside a unit of work and with each of their database statements
// failing in turn.
func TestSupplierQuotationDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	rfq := uuid.MustParse(e.publishedRFQ(t, "2026-07-31"))
	e.Must(201, "POST", "/v1/rfqs/"+rfq.String()+"/quotations", quotationBody(e.Vendor, "ANG-9", price(1, "400.00", "EUR", "")), "")
	for name, call := range map[string]func(ctx context.Context) error{
		"record quotation": func(ctx context.Context) error {
			_, err := h.CreateSupplierQuotation(ctx, &gen.SupplierQuotationCreate{SupplierID: e.Supplier, SupplierReference: "Q", ValidUntil: dateOf("2026-12-31"),
				Items: []gen.SupplierQuotationItemInput{{RfqItemNo: 1, UnitPrice: gen.Money{Amount: "1.00", Currency: "EUR"}, PriceUnit: gen.NewOptInt32(2)}}},
				gen.CreateSupplierQuotationParams{ID: rfq})
			return err
		},
		"list quotations": func(ctx context.Context) error {
			_, err := h.ListSupplierQuotations(ctx, gen.ListSupplierQuotationsParams{ID: rfq})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
}
