package endpoint_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

const drillPath = "/v1/analytics-documents"

// docs are a drill-through answer's documents as "kind number company
// amount currency".
func docs(res map[string]any) []string {
	var out []string
	for _, d := range res["documents"].([]any) {
		doc := d.(map[string]any)
		amount := doc["amount"].(map[string]any)
		out = append(out, fmt.Sprint(doc["kind"], " ", doc["number"], " ", doc["company"], " ", amount["amount"], " ", amount["currency"]))
		if doc["path"] != pathOf(doc["kind"].(string))+doc["id"].(string) {
			out = append(out, fmt.Sprint("path ", doc["path"]))
		}
	}
	return out
}

func pathOf(kind string) string {
	return map[string]string{"journal_entry": "/v1/journal-entries/", "purchase_order": "/v1/purchase-orders/", "supplier_invoice": "/v1/supplier-invoices/",
		"sales_order": "/v1/sales-orders/", "billing_document": "/v1/billing-documents/"}[kind]
}

// number is a document's number in table.
func number(t *testing.T, table string, id uuid.UUID) string {
	t.Helper()
	var n string
	pgtest.Scan(t, `SELECT document_number FROM `+table+` WHERE id = $1`, []any{id}, &n)
	return n
}

// TestDrillThrough: the documents behind a row are listed with their
// numbers, read as the caller, and add up to the row's figure, for every
// kind of document (T94b acceptance).
func TestDrillThrough(t *testing.T) {
	e := seeded(t, append([]string{"sales.order.read", "sales.billing.read"}, append(spendPerms, perms...)...)...)
	var revenue uuid.UUID
	pgtest.Scan(t, `SELECT journal_entry_id FROM ledger.journal_line l JOIN ledger.journal_entry j ON j.id = l.journal_entry_id
		WHERE j.tenant_id = $1 AND j.legal_entity_id = $2 LIMIT 1`, []any{e.Tenant.ID, e.DE}, &revenue)
	customer := e.Customer(t, "BP80000001", "Northwind Traders")
	order := e.SalesOrder(t, e.DE, customer, "EUR", "2026-03-12", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 150000}, fixture.SalesItem{Net: 23753})
	billing := e.BillingDocument(t, order, "2026-04-01", true)
	paper := e.Account(t, "2000", "610000")
	po := e.ContractOrder(t, e.DE, e.Supplier, nil, "EUR", "2026-03-01", paper, nil)
	invoice := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "posted", "EUR", "2026-03-06", fixture.InvoiceItem{Net: 10000, Order: &po},
		fixture.InvoiceItem{Net: 4000, Account: &paper})
	credit := e.SupplierDocument(t, e.DE, e.Supplier, "credit_memo", "posted", "EUR", "2026-03-07", fixture.InvoiceItem{Net: 1500, Account: &paper})
	for name, c := range map[string]struct {
		body string
		want []string
	}{
		"revenue": {`{"query": {"measures": ["revenue"], "by": ["company"]}, "row": {"company": "2000"}}`,
			[]string{"journal_entry " + number(t, "ledger.journal_entry", revenue) + " 2000 500.00 EUR"}},
		"spend": {`{"query": {"measures": ["spend", "off_contract_share"], "by": ["fiscal_month", "vendor"], "companies": ["2000"]}, "measure": "spend",
			"row": {"fiscal_month": "3", "vendor": "BP90000001"}}`, inCreationOrder(
			"supplier_invoice "+number(t, "payables.supplier_invoice", invoice)+" 2000 140.00 EUR",
			"supplier_invoice "+number(t, "payables.supplier_invoice", credit)+" 2000 -15.00 EUR")},
		"order intake": {`{"query": {"measures": ["order_intake"]}, "row": {}}`,
			[]string{"sales_order " + number(t, "sales.sales_order", order) + " 2000 1737.53 EUR"}},
		"billed value": {`{"query": {"measures": ["billed_value", "billing_document_count"], "by": ["customer_name"]}, "measure": "billed_value",
			"row": {"customer_name": "Northwind Traders"}}`, []string{"billing_document " + number(t, "sales.billing_document", billing) + " 2000 1737.53 EUR"}},
		"a count's documents": {`{"query": {"measures": ["purchase_order_count"], "where": [{"dim": "company", "op": "in", "values": ["2000"]}]}, "row": {}}`,
			[]string{"purchase_order " + number(t, "procurement.purchase_order", po) + " 2000 1.00 EUR"}},
		"invoices at gross": {`{"query": {"measures": ["invoice_value"], "by": ["invoice_status"]}, "row": {"invoice_status": "posted"}}`, inCreationOrder(
			"supplier_invoice "+number(t, "payables.supplier_invoice", invoice)+" 2000 140.00 EUR",
			"supplier_invoice "+number(t, "payables.supplier_invoice", credit)+" 2000 15.00 EUR")},
	} {
		t.Run(name, func(t *testing.T) {
			res, _ := e.Must(200, "POST", drillPath, c.body, "")
			if got := docs(res); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("documents: got %v, want %v", got, c.want)
			}
			if res["source"] != "database" || res["as_of"] == "" || res["config_version"] == nil || res["next_page_token"] != nil {
				t.Errorf("lineage: got %v as of %v under %v, next %v; want the database's, one page", res["source"], res["as_of"], res["config_version"], res["next_page_token"])
			}
		})
	}
	res, _ := e.Must(200, "POST", drillPath, `{"query": {"measures": ["spend"], "companies": ["2000"]}, "row": {}}`, "")
	if total := res["total"].(map[string]any); fmt.Sprint(total["value"], " ", total["currency"], " ", res["measure"]) != "125.00 EUR spend" {
		t.Errorf("total: got %v of %v, want the row's figure, 125.00 EUR of spend", total, res["measure"])
	}
	t.Run("pages", func(t *testing.T) { pages(t, e, drillPath) })
	t.Run("refusals", func(t *testing.T) { drillRefusals(t, e) })
}

func inCreationOrder(docs ...string) []string { return docs } // the fixture's ids are of version 7, in creation order

// pages: a page at a time lists what one page lists, each token bound to
// its request.
func pages(t *testing.T, e fixture.Env, path string) {
	t.Helper()
	body := `{"query": {"measures": ["invoice_value"], "companies": ["2000"]}, "row": {}}`
	all, _ := e.Must(200, "POST", path, body, "")
	first, _ := e.Must(200, "POST", path+"?page_size=1", body, "")
	token, _ := first["next_page_token"].(string)
	second, _ := e.Must(200, "POST", path+"?page_size=1&page_token="+url.QueryEscape(token), body, "")
	if got := fmt.Sprint(append(docs(first), docs(second)...)); got != fmt.Sprint(docs(all)) || second["next_page_token"] != nil {
		t.Errorf("pages: got %s then %v, want %v on two pages", got, second["next_page_token"], docs(all))
	}
	other := `{"query": {"measures": ["invoice_value"]}, "row": {}}`
	for name, c := range map[string]struct{ query, body string }{
		"another request": {"?page_token=" + url.QueryEscape(token), other},
		"a token altered": {"?page_token=" + url.QueryEscape(token+"x"), body},
	} {
		t.Run(name, func(t *testing.T) {
			if code, res, _ := e.Do("POST", path+c.query, c.body, ""); code != 400 || !strings.HasPrefix(fmt.Sprint(res["code"]), "request.invalid_page_") {
				t.Errorf("got %d %v, want 400 request.invalid_page_*", code, res)
			}
		})
	}
}

// drillRefusals: what cannot be drilled is 422 saying why; a caller sees
// the documents of the companies they may access alone, and needs the read
// of every kind of document a metric lists.
func drillRefusals(t *testing.T, e fixture.Env) {
	t.Helper()
	for name, c := range map[string]struct{ body, want string }{
		"derived":           {`{"query": {"measures": ["spend_off_contract"]}, "row": {}}`, "drill into spend or spend_under_contract"},
		"a row unnamed":     {`{"query": {"measures": ["revenue"], "by": ["company"]}, "row": {}}`, "the row needs its value of company"},
		"two currencies":    {`{"query": {"measures": ["revenue"]}, "row": {}}`, "would add amounts in EUR and USD"},
		"an unknown metric": {`{"query": {"measures": ["nope"]}, "row": {}}`, "unknown metric"},
	} {
		t.Run(name, func(t *testing.T) {
			code, res, _ := e.Do("POST", drillPath, c.body, "")
			if code != 422 || res["code"] != "analytics.invalid_query" || !strings.Contains(fmt.Sprint(res["detail"]), c.want) {
				t.Errorf("got %d %v, want 422 analytics.invalid_query saying %q", code, res, c.want)
			}
		})
	}
	t.Run("one company's reader", func(t *testing.T) {
		reader := e.As(e.Reader(t, "Reader", e.US))
		res, _ := reader.Must(200, "POST", drillPath, `{"query": {"measures": ["revenue"], "by": ["company"]}, "row": {"company": "2000"}}`, "")
		if got := docs(res); len(got) != 0 || res["total"].(map[string]any)["value"] != nil {
			t.Errorf("2000's revenue to a reader of 1000: got %v of %v, want no document and no figure", got, res["total"])
		}
	})
	for metric, lacking := range map[string]string{"revenue": "ledger.entry.read", "purchase_order_value": "procurement.order.read", "invoice_value": "payables.invoice.read",
		"spend": "payables.invoice.read", "order_intake": "sales.order.read", "billed_value": "sales.billing.read"} {
		t.Run(metric+" without "+lacking, func(t *testing.T) {
			var held []string
			for _, p := range append([]string{"sales.order.read", "sales.billing.read"}, append(spendPerms, perms...)...) {
				if p != lacking {
					held = append(held, p)
				}
			}
			other := seeded(t, held...)
			code, res, _ := other.Do("POST", drillPath, `{"query": {"measures": ["`+metric+`"]}, "row": {}}`, "")
			if code != 403 || res["code"] != "identity.permission_missing" {
				t.Errorf("got %d %v, want 403 identity.permission_missing", code, res)
			}
		})
	}
}
