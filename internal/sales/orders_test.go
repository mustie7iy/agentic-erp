package sales_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// accepted creates a quotation of the items to the customer and records
// its acceptance; it returns the quotation's id.
func (e env) accepted(t *testing.T, customer uuid.UUID, items ...string) string {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/quotations", e.body(customer, far, items...), "")
	id := got["id"].(string)
	e.Must(200, "POST", "/v1/quotations/"+id+":accept", "", `"1"`)
	return id
}

// direct is the JSON of an order of company 2000 in EUR created directly;
// extra is further fields, each led by a comma.
func (e env) direct(customer uuid.UUID, extra string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR"%s, "items": [%s]}`, e.DE, customer, extra, strings.Join(items, ", "))
}

func fromQuotation(id string) string { return fmt.Sprintf(`{"quotation_id": %q}`, id) }

// ordered creates an order of one item of 10.00 EUR directly and returns
// its id.
func (e env) ordered(t *testing.T) string {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{})), "")
	return got["id"].(string)
}

func quantity(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	q, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want a quantity", field, m[field])
	}
	return fmt.Sprintf("%v %v", q["value"], q["unit"])
}

// TestOrderFromQuotation: an order from an accepted quotation copies its
// customer, currency and items, takes the customer's payment term, and
// the quotation gives one order; it is audited and published; nothing is
// posted (T78b acceptance).
func TestOrderFromQuotation(t *testing.T) {
	e := setup(t)
	quotation := e.accepted(t, e.customer, e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1", key: "BRACKET-M8"}),
		e.item(line{quantity: "3", price: "99.99", priceUnit: 100, account: e.other}))
	quoted, _ := e.Must(200, "GET", "/v1/quotations/"+quotation, "", "")
	got, tag := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"status": "draft", "company_code": "2000", "company_name": "Acme GmbH", "customer_number": "BP70000001",
		"customer_name": "Northwind Traders", "currency": "EUR", "customer_id": e.customer.String(), "legal_entity_id": e.DE.String(),
		"quotation_id": quotation, "quotation_number": quoted["document_number"], "payment_term_code": "NT30"} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "SO") {
		t.Errorf("document number: got %v, want one of the sales_order range (SO…)", got["document_number"])
	}
	if a := amount(t, got, "net_amount"); a != "28.03 EUR" {
		t.Errorf("net value: got %s, want 28.03 EUR, the quotation's", a)
	}
	its, offered := items(t, got), items(t, quoted)
	if len(its) != 2 {
		t.Fatalf("items: got %d, want 2", len(its))
	}
	for i, want := range []struct{ ordered, none string }{{"2.5 KG", "0 KG"}, {"3 EA", "0 EA"}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			if q := quantity(t, its[i], "quantity"); q != want.ordered {
				t.Errorf("ordered: got %s, want %s", q, want.ordered)
			}
			if d := quantity(t, its[i], "delivered_quantity"); d != want.none {
				t.Errorf("delivered: got %s, want %s", d, want.none)
			}
			if b := quantity(t, its[i], "billed_quantity"); b != want.none {
				t.Errorf("billed: got %s, want %s", b, want.none)
			}
			copied := map[string]any{}
			for k, v := range its[i] {
				if k != "delivered_quantity" && k != "billed_quantity" {
					copied[k] = v
				}
			}
			if a, b := mustJSON(t, copied), mustJSON(t, offered[i]); a != b {
				t.Errorf("the item: got %s, want the quotation's %s", a, b)
			}
		})
	}
	id, _ := got["id"].(string)
	read, readTag := e.Must(200, "GET", "/v1/sales-orders/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	if readTag != tag {
		t.Errorf("ETag read back: got %s, want %s", readTag, tag)
	}
	if n := e.audits(t, "sales.sales_order.create", id); n != 1 {
		t.Errorf("audit records of the creation: got %d, want 1", n)
	}
	want := map[string]any{"sales_order_id": id, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "quotation_id": quotation,
		"order_date": got["order_date"], "payment_term_code": "NT30", "net_amount": map[string]any{"amount": "28.03", "currency": "EUR"},
		"status": "draft", "status_before": nil, "version": float64(1)}
	if a, b := mustJSON(t, e.events(t, "sales_order_changed", "sales_order_id", id)), mustJSON(t, []map[string]any{want}); a != b {
		t.Errorf("SalesOrderChanged: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	t.Run("one order per quotation", func(t *testing.T) {
		e.In(t).Problem(409, "sales.quotation_ordered", "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	})
	t.Run("two orders of one quotation at once", func(t *testing.T) {
		other := e.accepted(t, e.customer, e.item(line{}))
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/sales-orders", Body: fromQuotation(other), Tenant: e.Tenant.ID,
					As: e.Tenant.Human, Key: apitest.NewKey()}).Code
			})
		}
		wg.Wait()
		// The second waits for the first's lock on the quotation and then finds its order.
		slices.Sort(codes)
		if !slices.Equal(codes, []int{201, 409}) {
			t.Errorf("statuses: got %v, want one 201 and one 409", codes)
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1 AND quotation_id = $2`, []any{e.Tenant.ID, other}, &n)
		if n != 1 {
			t.Errorf("orders of the quotation: got %d, want 1", n)
		}
	})
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entries)
	if entries != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", entries)
	}
}

// TestOrderCreatedDirectly: an order from items is priced as a quotation
// is, its net value exact; its payment term is the one given, else the
// customer's (T78b acceptance).
func TestOrderCreatedDirectly(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, `, "payment_term_code": "NT14"`,
		e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1"}), e.item(line{quantity: "3", price: "99.99", priceUnit: 100})), "")
	if a := amount(t, got, "net_amount"); a != "28.03 EUR" {
		t.Errorf("net value: got %s, want 28.03 EUR (25.03 and 3.00)", a)
	}
	fields(t, got, map[string]any{"payment_term_code": "NT14", "quotation_id": nil, "quotation_number": nil, "status": "draft"})
	var nets []string
	for _, it := range items(t, got) {
		nets = append(nets, amount(t, it, "net_amount"))
	}
	if s := strings.Join(nets, ", "); s != "25.03 EUR, 3.00 EUR" {
		t.Errorf("items' net values: got %s, want 25.03 EUR, 3.00 EUR", s)
	}
	t.Run("the customer's term", func(t *testing.T) {
		own, _ := e.In(t).Must(201, "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{})), "")
		if own["payment_term_code"] != "NT30" {
			t.Errorf("payment term: got %v, want the customer's NT30", own["payment_term_code"])
		}
		ev := e.events(t, "sales_order_changed", "sales_order_id", own["id"].(string))
		if len(ev) != 1 {
			t.Fatalf("its events: got %d, want 1", len(ev))
		}
		fields(t, ev[0], map[string]any{"quotation_id": nil, "payment_term_code": "NT30"})
	})
	t.Run("idempotent", func(t *testing.T) {
		req := apitest.Req{Method: "POST", Path: "/v1/sales-orders", Body: e.direct(e.customer, "", e.item(line{price: "77.00"})),
			Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
		first, second := e.API.Do(req), e.API.Do(req)
		if first.Code != 201 {
			t.Fatalf("first status: got %d, want 201: %s", first.Code, first.Body)
		}
		if second.Code != 201 {
			t.Fatalf("second status: got %d, want 201: %s", second.Code, second.Body)
		}
		if a, b := apitest.JSON(t, first)["id"], apitest.JSON(t, second)["id"]; a != b {
			t.Errorf("replay: got order %v, want the first's %v", b, a)
		}
		if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
			t.Errorf("Idempotent-Replayed: got %q, want true", h)
		}
	})
}

// TestOrderRefusals: every refusal of an order names the request's own
// field where one caused it, and nothing is written (T78b acceptance).
func TestOrderRefusals(t *testing.T) {
	e := setup(t)
	ok := e.item(line{})
	open := e.quoted(t)
	rejected := e.quoted(t)
	e.Must(200, "POST", "/v1/quotations/"+rejected+":decline", "", `"1"`)
	// A customer without data in the company can be quoted to, not sold to.
	prospect := e.partner(t, "BP70000004", "Prospect", "active", "customer")
	prospects := e.accepted(t, prospect, ok)
	theirs := e.accepted(t, e.customer, ok)
	without := func(field string) string {
		body := e.direct(e.customer, "", ok)
		for _, part := range []string{fmt.Sprintf(`"legal_entity_id": %q, `, e.DE), fmt.Sprintf(`"customer_id": %q, `, e.customer), `"currency": "EUR", `} {
			if strings.HasPrefix(part, `"`+field+`"`) {
				return strings.Replace(body, part, "", 1)
			}
		}
		return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR"}`, e.DE, e.customer)
	}
	with := func(extra string) string { return strings.Replace(fromQuotation(theirs), "}", ", "+extra+"}", 1) }
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"an unknown quotation", fromQuotation(uuid.NewString()), 422, "request.reference_not_found", "quotation_id"},
		{"an open quotation", fromQuotation(open), 409, "sales.quotation_not_accepted", nil},
		{"a rejected quotation", fromQuotation(rejected), 409, "sales.quotation_not_accepted", nil},
		{"a quotation and a company", with(fmt.Sprintf(`"legal_entity_id": %q`, e.DE)), 422, "sales.quotation_or_items", "legal_entity_id"},
		{"a quotation and a customer", with(fmt.Sprintf(`"customer_id": %q`, e.customer)), 422, "sales.quotation_or_items", "customer_id"},
		{"a quotation and a currency", with(`"currency": "EUR"`), 422, "sales.quotation_or_items", "currency"},
		{"a quotation and items", with(`"items": [` + ok + `]`), 422, "sales.quotation_or_items", "items"},
		{"neither a quotation nor a company", without("legal_entity_id"), 422, "sales.quotation_or_items", "legal_entity_id"},
		{"neither a quotation nor a customer", without("customer_id"), 422, "sales.quotation_or_items", "customer_id"},
		{"neither a quotation nor a currency", without("currency"), 422, "sales.quotation_or_items", "currency"},
		{"neither a quotation nor items", without("items"), 422, "sales.quotation_or_items", "items"},
		{"a quotation's customer without company data", fromQuotation(prospects), 422, "sales.customer_not_usable", "quotation_id"},
		{"a customer without company data", e.direct(prospect, "", ok), 422, "sales.customer_not_usable", "customer_id"},
		{"an unknown customer", e.direct(uuid.New(), "", ok), 422, "request.reference_not_found", "customer_id"},
		{"an unknown company", strings.Replace(e.direct(e.customer, "", ok), e.DE.String(), uuid.NewString(), 1), 422, "request.reference_not_found", "legal_entity_id"},
		{"an unknown currency", strings.ReplaceAll(e.direct(e.customer, "", ok), "EUR", "XXA"), 422, "sales.unknown_currency", "currency"},
		{"an unknown payment term", e.direct(e.customer, `, "payment_term_code": "NOPE"`, ok), 422, "sales.unknown_payment_term", "payment_term_code"},
		{"an unknown payment term with a quotation", with(`"payment_term_code": "NOPE"`), 422, "sales.unknown_payment_term", "payment_term_code"},
		{"an unknown unit", e.direct(e.customer, "", ok, e.item(line{unit: "XX"})), 422, "sales.unknown_unit", "items[1].quantity.unit"},
		{"a price of zero", e.direct(e.customer, "", e.item(line{price: "0.00"})), 422, "sales.amount_not_positive", "items[0].unit_price.amount"},
		{"an unknown tax code", e.direct(e.customer, "", e.item(line{code: "ZZ"})), 422, "sales.unknown_tax_code", "items[0].tax_code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", "/v1/sales-orders", c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("orders after the refusals: got %d, want none", n)
	}
	t.Run("no items", func(t *testing.T) {
		// The contract refuses an empty list before the handler; the port refuses it too.
		eur := "EUR"
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t, time.Now()).MakeOrder(ctx, sales.OrderInput{LegalEntityID: &e.DE, CustomerID: &e.customer, Currency: &eur,
				Items: []sales.ItemInput{}})
			return err
		})
		refusal(t, err, 422, "sales.items_required")
	})
	t.Run("the term given with a quotation", func(t *testing.T) {
		got, _ := e.In(t).Must(201, "POST", "/v1/sales-orders", with(`"payment_term_code": "NT14"`), "")
		if got["payment_term_code"] != "NT14" {
			t.Errorf("payment term: got %v, want NT14", got["payment_term_code"])
		}
	})
	t.Run("another company's quotation", func(t *testing.T) {
		other := e.accepted(t, e.customer, ok)
		reader := e.As(e.Reader(t, "US sales", e.US))
		if f := field(reader.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/sales-orders", fromQuotation(other), "")); f != "quotation_id" {
			t.Errorf("field: got %v, want quotation_id", f)
		}
	})
}

// TestOrderConfirmed: a draft is confirmed once, at the version the
// caller read; a replay answers the same; the audit record and
// SalesOrderChanged are written (T78b acceptance).
func TestOrderConfirmed(t *testing.T) {
	e := setup(t)
	id := e.ordered(t)
	path := "/v1/sales-orders/" + id + ":confirm"
	e.Problem(428, "request.precondition_required", "POST", path, "", "")
	e.Problem(412, "request.version_mismatch", "POST", path, "", `"7"`)
	req := apitest.Req{Method: "POST", Path: path, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	fields(t, a, map[string]any{"status": "confirmed", "version": float64(2)})
	if tag := first.Header().Get("ETag"); tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	if mustJSON(t, a) != mustJSON(t, b) {
		t.Errorf("replay: got %v, want the first answer %v", b, a)
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	if n := e.audits(t, "sales.sales_order.confirm", id); n != 1 {
		t.Errorf("audit records of the confirmation: got %d, want 1", n)
	}
	events := e.events(t, "sales_order_changed", "sales_order_id", id)
	if len(events) != 2 {
		t.Fatalf("SalesOrderChanged events: got %d, want 2 (created, confirmed)", len(events))
	}
	fields(t, events[1], map[string]any{"status": "confirmed", "status_before": "draft", "version": float64(2), "customer_number": "BP70000001"})
	t.Run("confirmed once", func(t *testing.T) {
		e.In(t).Problem(409, "sales.invalid_status", "POST", path, "", `"2"`)
	})
	t.Run("two confirmations at once", func(t *testing.T) {
		other := e.ordered(t)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/sales-orders/" + other + ":confirm", Tenant: e.Tenant.ID,
					As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}).Code
			})
		}
		wg.Wait()
		// The second waits for the first's row lock and then finds the order at another version.
		slices.Sort(codes)
		if !slices.Equal(codes, []int{200, 412}) {
			t.Errorf("statuses: got %v, want one 200 and one 412", codes)
		}
		if ev := e.events(t, "sales_order_changed", "sales_order_id", other); len(ev) != 2 {
			t.Errorf("events of two confirmations at once: got %d, want 2 (created, confirmed)", len(ev))
		}
	})
	t.Run("an unknown order", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", "/v1/sales-orders/"+uuid.NewString()+":confirm", "", `"1"`)
	})
	t.Run("without the permission to confirm", func(t *testing.T) {
		other := e.ordered(t)
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.order.confirm'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/sales-orders/"+other+":confirm", "", `"1"`)
	})
}

// TestOrdersListed: orders are listed by customer, status, company,
// number and quotation, in pages, and only those of the companies the
// caller may act in.
func TestOrdersListed(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	e.companyData(t, other, e.DE, "NT14")
	quotation := e.accepted(t, e.customer, e.item(line{}))
	first := e.ordered(t)
	second, _ := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	theirs, _ := e.Must(201, "POST", "/v1/sales-orders", e.direct(other, "", e.item(line{})), "")
	e.Must(200, "POST", "/v1/sales-orders/"+first+":confirm", "", `"1"`)
	ids := func(t *testing.T, c apitest.Client, query string) string {
		t.Helper()
		got, _ := c.In(t).Must(200, "GET", "/v1/sales-orders?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, it["id"].(string))
		}
		return strings.Join(out, " ")
	}
	sid, tid := second["id"].(string), theirs["id"].(string)
	for _, c := range []struct{ name, query, want string }{
		{"a customer's, by number", "customer_id=" + e.customer.String() + "&order_by=document_number", first + " " + sid},
		{"a customer's drafts", "customer_id=" + e.customer.String() + "&status=draft", sid},
		{"the confirmed", "status=confirmed", first},
		{"another customer's", "customer_id=" + other.String(), tid},
		{"a company's, newest number first", "legal_entity_id=" + e.DE.String() + "&order_by=document_number%20desc&page_size=2", tid + " " + sid},
		{"another company's", "legal_entity_id=" + e.US.String(), ""},
		{"by number", "document_number=" + second["document_number"].(string), sid},
		{"a quotation's", "quotation_id=" + quotation, sid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ids(t, e.Client, c.query); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		// The three are dated the same day: the id breaks the tie, so the pages hold each once.
		for _, order := range []string{"", "&order_by=document_number"} {
			query := "page_size=2" + order
			page, _ := e.In(t).Must(200, "GET", "/v1/sales-orders?"+query, "", "")
			token, _ := page["next_page_token"].(string)
			if token == "" {
				t.Fatalf("first page (%s): got no next_page_token, want one", query)
			}
			all := strings.Fields(ids(t, e.Client, query) + " " + ids(t, e.Client, query+"&page_token="+token))
			slices.Sort(all)
			want := []string{first, sid, tid}
			slices.Sort(want)
			if !slices.Equal(all, want) {
				t.Errorf("the two pages (%s): got %v, want %v", query, all, want)
			}
		}
	})
	t.Run("only the caller's companies", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		if got := ids(t, reader, ""); got != "" {
			t.Errorf("listed: got %s, want none", got)
		}
		reader.In(t).Problem(404, "request.not_found", "GET", "/v1/sales-orders/"+first, "", "")
		reader.In(t).Problem(404, "request.not_found", "POST", "/v1/sales-orders/"+sid+":confirm", "", `"1"`)
		reader.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/sales-orders", e.direct(e.customer, "", e.item(line{})), "")
	})
}

// TestOrdersPublishedRead: sales.Orders gives an order with its items to
// the contexts that read sales, and says when there is none.
func TestOrdersPublishedRead(t *testing.T) {
	e := setup(t)
	quotation := e.accepted(t, e.customer, e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1", key: "BRACKET-M8"}))
	created, _ := e.Must(201, "POST", "/v1/sales-orders", fromQuotation(quotation), "")
	id := uuid.MustParse(created["id"].(string))
	err := e.inRequest(t, func(ctx context.Context) error {
		o, ok, err := sales.Orders{}.Order(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("got no order, want the order created")
		}
		if len(o.Items) != 1 {
			t.Fatalf("items: got %d, want 1", len(o.Items))
		}
		it := o.Items[0]
		text := func(p *string) any {
			if p == nil {
				return nil
			}
			return *p
		}
		id := func(p *uuid.UUID) any {
			if p == nil {
				return nil
			}
			return p.String()
		}
		for _, c := range []struct {
			name      string
			got, want any
		}{
			{"status", o.Status, sales.Draft}, {"net value", int64(o.Net), int64(2503)}, {"currency", o.Currency, "EUR"}, {"exponent", o.Exponent, 2},
			{"payment term", o.PaymentTermCode, "NT30"}, {"customer", o.CustomerID, e.customer}, {"quotation", id(o.QuotationID), quotation},
			{"quotation number", text(o.QuotationNumber), created["quotation_number"]},
			{"item's quantity", it.Quantity, "2.5"}, {"item's unit", it.Unit, "KG"}, {"item's unit price", int64(it.UnitPrice), int64(1001)},
			{"item's price unit", it.PriceUnit, int32(1)}, {"item's net value", int64(it.Net), int64(2503)}, {"item's account", it.AccountID, e.revenue},
			{"item's delivered quantity", it.Delivered, "0"}, {"item's billed quantity", it.Billed, "0"}, {"item's tax code", text(it.TaxCode), "A1"},
			{"item's key", text(it.ItemKey), "BRACKET-M8"},
		} {
			if c.got != c.want {
				t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
			}
		}
		_, ok, err = sales.Orders{}.Order(ctx, uuid.New())
		if err != nil {
			return err
		}
		if ok {
			t.Error("an unknown order: got one, want none")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := (sales.Orders{}).Order(context.Background(), id); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "read an order", func(ctx context.Context) error {
		_, _, err := sales.Orders{}.Order(ctx, id)
		return err
	})
}

// TestOrderDatedInTheCompanysZone: an order is dated the company's date
// in its time zone, from a quotation and directly.
func TestOrderDatedInTheCompanysZone(t *testing.T) {
	e := setup(t)
	quotation := uuid.MustParse(e.accepted(t, e.customer, e.item(line{})))
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	currency := "EUR"
	for name, in := range map[string]sales.OrderInput{
		"from a quotation": {QuotationID: &quotation},
		"directly": {LegalEntityID: &e.DE, CustomerID: &e.customer, Currency: &currency, Items: []sales.ItemInput{{Description: "Bracket",
			Quantity: "1", Unit: "EA", UnitPrice: sales.Amount{Value: "10.00", Currency: "EUR"}, PriceUnit: 1, AccountID: e.revenue}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := e.inRequest(t, func(ctx context.Context) error {
				o, err := h.MakeOrder(ctx, in)
				if err == nil && o.OrderDate.Format(time.DateOnly) != "2026-03-09" {
					t.Errorf("order date: got %s, want 2026-03-09, Toronto's date", o.OrderDate.Format(time.DateOnly))
				}
				return err
			})
			if err != nil {
				t.Errorf("got %v, want the order created", err)
			}
		})
	}
}
