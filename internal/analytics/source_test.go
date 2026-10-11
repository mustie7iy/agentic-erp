package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
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
		"/v1/open-items": {items(map[string]any{"journal_entry_id": "j1", "line_no": 2})},
		"/v1/journal-entries": {items(map[string]any{"id": "j1", "document_type": "KR", "posting_date": "2026-03-02", "lines": []any{
			map[string]any{"line_no": 1, "account_id": "a2", "amount_lc": map[string]any{"amount": "10.00", "currency": "USD"}},
			map[string]any{"line_no": 2, "account_id": "a1", "partner_id": "p2", "amount_lc": map[string]any{"amount": "-10.00", "currency": "USD"}}}})},
		"/v1/purchase-orders": {items(map[string]any{"id": "o1", "supplier_id": "p1", "order_date": "2026-03-01", "items": []any{
			map[string]any{"quantity": map[string]any{"value": "2"}, "unit_price": map[string]any{"amount": "5", "currency": "JPY"}, "price_unit": 1, "account_id": "a2"}}})},
		"/v1/supplier-invoices": {items(map[string]any{"id": "i1", "supplier_id": "p2", "invoice_date": "2026-03-05", "status": "parked",
			"gross_amount": map[string]any{"amount": "10.00", "currency": "USD"}})},
	}
}

func TestSourceReadsEveryPage(t *testing.T) {
	m, _ := DefaultModel()
	f, err := Source{API: fakeAPI(t, validPages())}.Facts(context.Background(), m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gl, po, inv := f.Tables["gl_lines"], f.Tables["po_lines"], f.Tables["ap_invoices"]
	if len(gl) != 2 || gl[1].Dims["vendor_name"] != "Beta" || gl[1].Dims["cleared"] != "false" || gl[0].Dims["cleared"] != "false" ||
		gl[1].Amount != -1000 || gl[0].Dims["fiscal_month"] != "3" {
		t.Fatalf("gl: %+v", gl)
	}
	if len(po) != 1 || po[0].Amount != 10 || po[0].Currency != "JPY" || po[0].Dims["account_name"] != "Office" {
		t.Fatalf("po: %+v", po)
	}
	if len(inv) != 1 || inv[0].Dims["invoice_status"] != "parked" || f.DataThrough != "2026-03-05" || f.Companies["1000"] != "USD" {
		t.Fatalf("invoices: %+v %s", inv, f.DataThrough)
	}
}

func TestSourceErrors(t *testing.T) {
	m, _ := DefaultModel()
	for _, path := range []string{"/v1/legal-entities", "/v1/business-partners", "/v1/accounts", "/v1/open-items", "/v1/journal-entries",
		"/v1/purchase-orders", "/v1/supplier-invoices"} {
		pages := validPages()
		delete(pages, path)
		if _, err := (Source{API: fakeAPI(t, pages)}).Facts(context.Background(), m, time.Now()); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s refused: %v", path, err)
		}
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
	} {
		pages := validPages()
		fix(pages)
		if _, err := (Source{API: fakeAPI(t, pages)}).Facts(context.Background(), m, time.Now()); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := (Source{API: &apitools.Client{BaseURL: "http://127.0.0.1:1"}}).Facts(context.Background(), m, time.Now()); err == nil {
		t.Fatal("read without a server")
	}
}
