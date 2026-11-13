package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// fakeAPI answers list operations from canned pages: path -> pages, the
// next page asked for with page_token=<index>.
func fakeAPI(t *testing.T, pages map[string][]string) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(403)
			w.Write([]byte(`{"code": "identity.permission_missing"}`))
			return
		}
		i := 0
		if tok := r.URL.Query().Get("page_token"); tok != "" {
			i = int(tok[0] - '0')
		}
		w.Write([]byte(ps[i]))
	}))
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

func items(v ...any) string {
	b, _ := json.Marshal(map[string]any{"items": v})
	return string(b)
}

func validPages() map[string][]string {
	return map[string][]string{
		"/v1/legal-entities":    {items(map[string]any{"id": "le", "code": "1000", "local_currency": "USD"})},
		"/v1/business-partners": {`{"items": [{"id": "p1", "number": "BP1", "name": "Acme"}], "next_page_token": "1"}`, items(map[string]any{"id": "p2", "number": "BP2", "name": "Beta"})},
		"/v1/accounts": {items(map[string]any{"id": "a1", "number": "211000", "name": "AP", "account_type": "liability", "reconciliation_kind": "supplier"},
			map[string]any{"id": "a2", "number": "520000", "name": "Office", "account_type": "expense", "reconciliation_kind": "none"})},
		"/v1/ledgers": {items(map[string]any{"id": "0l", "is_leading": true, "legal_entity_ids": []any{"le"}},
			map[string]any{"id": "2l", "is_leading": false, "legal_entity_ids": []any{}})},
		"/v1/open-items": {items(map[string]any{"journal_entry_id": "j1", "line_no": 2})},
		"/v1/journal-entries": {items(map[string]any{"id": "j1", "document_type": "KR", "posting_date": "2026-03-02", "lines": []any{
			map[string]any{"line_no": 1, "account_id": "a2", "amount_lc": map[string]any{"amount": "10.00", "currency": "USD"}},
			map[string]any{"line_no": 2, "account_id": "a1", "partner_id": "p2", "amount_lc": map[string]any{"amount": "-10.00", "currency": "USD"}}}})},
		"/v1/purchase-orders": {items(map[string]any{"id": "o1", "supplier_id": "p1", "order_date": "2026-03-01", "items": []any{
			map[string]any{"quantity": map[string]any{"value": "2"}, "unit_price": map[string]any{"amount": "5", "currency": "JPY"}, "price_unit": 1, "account_id": "a2"}}})},
		"/v1/supplier-invoices": {items(map[string]any{"id": "i1", "supplier_id": "p2", "invoice_date": "2026-03-05", "status": "parked",
			"gross_amount": map[string]any{"amount": "10.00", "currency": "USD"}})},
		"/v1/sales-orders": {items(map[string]any{"id": "s1", "customer_id": "p2", "order_date": "2026-03-03", "items": []any{
			map[string]any{"item_key": "BRACKET", "net_amount": map[string]any{"amount": "30.00", "currency": "USD"}},
			map[string]any{"net_amount": map[string]any{"amount": "12.50", "currency": "USD"}}}})},
		"/v1/billing-documents": {items(map[string]any{"id": "b1", "customer_id": "p2", "billing_date": "2026-03-04", "items": []any{
			map[string]any{"item_key": "BRACKET", "net_amount": map[string]any{"amount": "20.00", "currency": "USD"}}}})},
	}
}

// TestSourceReadsSalesLines: a confirmed order's items and a posted
// billing document's are facts of sales_lines, each dated its document's
// date, naming its customer as /v1/business-partners does and its item
// key; the lists are asked for those statuses alone (T91a).
func TestSourceReadsSalesLines(t *testing.T) {
	m, _ := DefaultModel()
	asked := map[string]string{}
	api := fakeAPI(t, validPages())
	api.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked[r.URL.Path] = r.URL.Query().Get("status") + " of " + r.URL.Query().Get("legal_entity_id")
		return http.DefaultTransport.RoundTrip(r)
	})}
	f, err := Source{API: api, Datasets: []string{"sales_lines"}}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/v1/sales-orders": "confirmed of le", "/v1/billing-documents": "posted of le"} {
		if asked[path] != want {
			t.Errorf("%s: asked for %q, want %q", path, asked[path], want)
		}
	}
	for _, path := range []string{"/v1/accounts", "/v1/journal-entries", "/v1/purchase-orders", "/v1/supplier-invoices"} {
		if _, read := asked[path]; read {
			t.Errorf("%s: read, want only the sales documents' reads", path)
		}
	}
	var got []string
	for _, l := range f.Tables["sales_lines"] {
		d := l.Dims
		got = append(got, strings.Join([]string{l.ID, d["sales_document"], d["company"], d["document_date"], d["fiscal_month"], d["customer"], d["customer_name"],
			d["item_key"], d["currency"], l.Amount.Plain(2), l.Currency}, " "))
	}
	want := []string{"s1 order 1000 2026-03-03 3 BP2 Beta BRACKET USD 30.00 USD", "s1 order 1000 2026-03-03 3 BP2 Beta  USD 12.50 USD",
		"b1 billing 1000 2026-03-04 3 BP2 Beta BRACKET USD 20.00 USD"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sales lines:\n got %q\nwant %q", got, want)
	}
	if f.DataThrough != "2026-03-04" {
		t.Errorf("data through: got %s, want 2026-03-04", f.DataThrough)
	}
	for metric, want := range map[string]string{"order_intake": "42.50", "billed_value": "20.00", "billing_document_count": "1"} {
		res, err := Execute(m, f, Principal{}, Query{Measures: []string{metric}, By: []string{"customer_name"}})
		if err != nil {
			t.Errorf("%s by customer: got %v, want an answer", metric, err)
			continue
		}
		if got := fmt.Sprint(res.Rows); got != "[[Beta "+want+"]]" {
			t.Errorf("%s by customer: got %s, want [[Beta %s]]", metric, got, want)
		}
	}
}

func TestSourceReadsEveryPage(t *testing.T) {
	m, _ := DefaultModel()
	f, err := Source{API: fakeAPI(t, validPages())}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gl, po, inv := f.Tables["gl_lines"], f.Tables["po_lines"], f.Tables["ap_invoices"]
	if len(gl) != 2 {
		t.Fatalf("gl: got %d lines %+v, want 2", len(gl), gl)
	}
	for _, c := range []struct {
		line      int
		dim, want string
	}{{1, "vendor_name", "Beta"}, {1, "cleared", "false"}, {0, "cleared", "false"}, {0, "fiscal_month", "3"}} {
		if got := gl[c.line].Dims[c.dim]; got != c.want {
			t.Errorf("gl[%d] %s: got %q, want %q", c.line, c.dim, got, c.want)
		}
	}
	if gl[1].Amount != -1000 {
		t.Errorf("gl[1] amount: got %v, want -1000", gl[1].Amount)
	}
	if len(po) != 1 {
		t.Fatalf("po: got %d lines %+v, want 1", len(po), po)
	}
	if po[0].Amount != 10 {
		t.Errorf("po amount: got %v, want 10", po[0].Amount)
	}
	if po[0].Currency != "JPY" {
		t.Errorf("po currency: got %s, want JPY", po[0].Currency)
	}
	if got := po[0].Dims["account_name"]; got != "Office" {
		t.Errorf("po account_name: got %q, want Office", got)
	}
	if len(inv) != 1 {
		t.Fatalf("invoices: got %d %+v, want 1", len(inv), inv)
	}
	if got := inv[0].Dims["invoice_status"]; got != "parked" {
		t.Errorf("invoice status: got %q, want parked", got)
	}
	if f.DataThrough != "2026-03-05" {
		t.Errorf("data through: got %s, want 2026-03-05", f.DataThrough)
	}
	if got := f.Companies["1000"]; got != "USD" {
		t.Errorf("currency of company 1000: got %s, want USD", got)
	}
	// The API writes each amount with its currency's digits from
	// ref.currency, so those digits are the exponents.
	for cur, want := range map[string]int{"USD": 2, "JPY": 0} {
		if got, ok := f.Exponents[cur]; !ok || got != want {
			t.Errorf("exponent of %s: got %d (%v), want %d", cur, got, ok, want)
		}
	}
}

// TestSourceReadsParallelLedgers: the journal list reads one ledger, the
// leading one by default, so a legal entity keeping a parallel ledger has
// it read too, by its id (ADR-0045).
func TestSourceReadsParallelLedgers(t *testing.T) {
	m, _ := DefaultModel()
	pages := validPages()
	pages["/v1/ledgers"] = []string{items(map[string]any{"id": "0l", "is_leading": true, "legal_entity_ids": []any{"le"}},
		map[string]any{"id": "2l", "is_leading": false, "legal_entity_ids": []any{"le"}})}
	var asked []string
	api := fakeAPI(t, pages)
	api.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/journal-entries" {
			asked = append(asked, r.URL.Query().Get("ledger_id"))
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	f, err := Source{API: api}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(asked, ","); got != ",2l" {
		t.Errorf("journal lists: got ledgers %q, want the default and 2l", got)
	}
	if got := len(f.Tables["gl_lines"]); got != 4 {
		t.Errorf("gl lines: got %d, want both ledgers' 2", got)
	}
}

// roundTrip is an http.RoundTripper of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An amount whose digits disagree with an earlier one in its currency is
// refused: rendering with either exponent could be wrong.
func TestSourceRefusesInconsistentDigits(t *testing.T) {
	m, _ := DefaultModel()
	pages := validPages()
	pages["/v1/supplier-invoices"][0] = strings.Replace(pages["/v1/supplier-invoices"][0], `"10.00"`, `"10.000"`, 1)
	_, err := (Source{API: fakeAPI(t, pages)}).Facts(context.Background(), m, time.Now())
	pgtest.CheckErr(t, err, "invoice i1: amount 10.000 USD has 3 decimals, other USD amounts 2")
}

// A document quantity has up to six decimals (money.QuantityDecimals), as
// the API and procurement allow. An order line of 0.0005 KG at 1000.00 per
// unit is worth 0.50; read with three decimals, it failed the tenant's
// whole fact build.
func TestSourceReadsSixDecimalQuantities(t *testing.T) {
	m, _ := DefaultModel()
	pages := validPages()
	pages["/v1/purchase-orders"] = []string{items(map[string]any{"id": "o1", "supplier_id": "p1", "order_date": "2026-03-01", "items": []any{
		map[string]any{"quantity": map[string]any{"value": "0.0005"}, "unit_price": map[string]any{"amount": "1000.00", "currency": "USD"},
			"price_unit": 1, "account_id": "a2"}}})}
	f, err := Source{API: fakeAPI(t, pages)}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatalf("facts: got %v, want no error", err)
	}
	po := f.Tables["po_lines"]
	if len(po) != 1 {
		t.Fatalf("po lines: got %d %+v, want 1", len(po), po)
	}
	if po[0].Amount != 50 {
		t.Errorf("po amount: got %v, want 50", po[0].Amount)
	}
}

func TestSourceErrors(t *testing.T) {
	m, _ := DefaultModel()
	for _, path := range []string{"/v1/legal-entities", "/v1/business-partners", "/v1/ledgers", "/v1/accounts", "/v1/open-items", "/v1/journal-entries",
		"/v1/purchase-orders", "/v1/supplier-invoices", "/v1/sales-orders", "/v1/billing-documents"} {
		t.Run("without "+path, func(t *testing.T) {
			pages := validPages()
			delete(pages, path)
			_, err := (Source{API: fakeAPI(t, pages)}).Facts(context.Background(), m, time.Now())
			pgtest.CheckErr(t, err, path)
		})
	}
	for name, fix := range map[string]func(p map[string][]string){
		"journal amount": func(p map[string][]string) {
			p["/v1/journal-entries"][0] = strings.Replace(p["/v1/journal-entries"][0], "-10.00", "x", 1)
		},
		"journal date": func(p map[string][]string) {
			p["/v1/journal-entries"][0] = strings.Replace(p["/v1/journal-entries"][0], "2026-03-02", "x", 1)
		},
		"order quantity": func(p map[string][]string) {
			p["/v1/purchase-orders"][0] = strings.Replace(p["/v1/purchase-orders"][0], `"2"`, `"x"`, 1)
		},
		"order price": func(p map[string][]string) {
			p["/v1/purchase-orders"][0] = strings.Replace(p["/v1/purchase-orders"][0], `"5"`, `"x"`, 1)
		},
		"order date": func(p map[string][]string) {
			p["/v1/purchase-orders"][0] = strings.Replace(p["/v1/purchase-orders"][0], "2026-03-01", "x", 1)
		},
		"invoice amount": func(p map[string][]string) {
			p["/v1/supplier-invoices"][0] = strings.Replace(p["/v1/supplier-invoices"][0], "10.00", "x", 1)
		},
		"invoice date": func(p map[string][]string) {
			p["/v1/supplier-invoices"][0] = strings.Replace(p["/v1/supplier-invoices"][0], "2026-03-05", "x", 1)
		},
		"sales order amount": func(p map[string][]string) {
			p["/v1/sales-orders"][0] = strings.Replace(p["/v1/sales-orders"][0], "12.50", "x", 1)
		},
		"billing date": func(p map[string][]string) {
			p["/v1/billing-documents"][0] = strings.Replace(p["/v1/billing-documents"][0], "2026-03-04", "x", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			pages := validPages()
			fix(pages)
			if _, err := (Source{API: fakeAPI(t, pages)}).Facts(context.Background(), m, time.Now()); err == nil {
				t.Error("got no error, want the page refused")
			}
		})
	}
	if _, err := (Source{API: &apitools.Client{BaseURL: "http://127.0.0.1:1"}}).Facts(context.Background(), m, time.Now()); err == nil {
		t.Fatal("read without a server")
	}
}

// TestSourcePermissions: the permissions are those of the reads each
// selection of fact tables makes, in the order they are made.
func TestSourcePermissions(t *testing.T) {
	for name, c := range map[string]struct {
		datasets []string
		want     string
	}{
		"every table":  {nil, "tenancy.org.read partner.supplier.read ledger.account.read ledger.entry.read procurement.order.read payables.invoice.read sales.order.read sales.billing.read cash.forecast.read"},
		"sales":        {[]string{"sales_lines"}, "tenancy.org.read partner.supplier.read sales.order.read sales.billing.read"},
		"cash actuals": {[]string{"cash_actuals"}, "tenancy.org.read cash.forecast.read"},
		"journal":      {[]string{"gl_lines"}, "tenancy.org.read partner.supplier.read ledger.account.read ledger.entry.read"},
		"orders":       {[]string{"po_lines"}, "tenancy.org.read partner.supplier.read ledger.account.read procurement.order.read"},
		"invoices":     {[]string{"ap_invoices"}, "tenancy.org.read partner.supplier.read payables.invoice.read"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(Source{Datasets: c.datasets}.Permissions(), " "); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
