package receivables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// jurisdictionLines are the entry's lines with the jurisdiction each
// names, by line number: account, amount, tax code and jurisdiction.
func (e env) jurisdictionLines(t *testing.T, entry any) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s %s %s %s', a.number, l.amount_tc, coalesce(l.tax_code, '-'), coalesce(l.tax_jurisdiction, '-')),
			'; ' ORDER BY l.line_no), '')
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// taxItems are the entry's tax items, by code and jurisdiction: what a
// tax return reads (ADR-0049 §5, §11).
func (e env) taxItems(t *testing.T, entry any) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s %s %s %s', code, coalesce(jurisdiction, '-'), base_tc, tax_tc), '; '
			ORDER BY code, jurisdiction, item_no), '')
		FROM ledger.tax_item WHERE tenant_id = $1 AND journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// emirates is an invoice of three items of AJ, two in DE-BY and one in
// DE-BE, and one of A1, which levies no line's subdivision. 5% of 143.48
// is 7.174: 7.17 once for the code, shared out 5.00, 1.67 and 0.50, so
// DE-BY bears 5.50 and DE-BE 1.67; A1 gives 19.00 on 100.00.
func (e env) emirates() string {
	return e.invoice(e.customer, "2026-03-10", in(e.item("100.10", "AJ"), "DE-BY"), in(e.item("33.33", "AJ"), "DE-BE"),
		in(e.item("10.05", "AJ"), "DE-BY"), e.item("100.00", "A1"))
}

// TestItemJurisdiction: an item whose code levies its lines' subdivision
// names it and reads back with it; the invoice's tax is the code's, each
// item keeping its share; an item of another code shows none (product
// owner 2026-11-12; ADR-0063 §4 As built).
func TestItemJurisdiction(t *testing.T) {
	e := setup(t)
	got, _ := e.Must(201, "POST", "/v1/customer-invoices", e.emirates(), "")
	for f, want := range map[string]string{"net_amount": "243.48 EUR", "tax_amount": "26.17 EUR", "gross_amount": "269.65 EUR"} {
		if a := amount(t, got, f); a != want {
			t.Errorf("%s: got %s, want %s", f, a, want)
		}
	}
	want := []string{"AJ DE-BY 5.00 EUR", "AJ DE-BE 1.67 EUR", "AJ DE-BY 0.50 EUR", "A1 <nil> 19.00 EUR"}
	its := items(t, got)
	if len(its) != len(want) {
		t.Fatalf("items: got %d, want %d", len(its), len(want))
	}
	for i, it := range its {
		if s := fmt.Sprintf("%v %v %s", it["tax_code"], it["tax_jurisdiction"], amount(t, it, "tax_amount")); s != want[i] {
			t.Errorf("item %d: got %s, want %s", i+1, s, want[i])
		}
	}
	read, _ := e.Must(200, "GET", "/v1/customer-invoices/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	guarded, _ := e.Must(201, "POST", "/v1/customer-invoices", e.invoice(e.customer, "2026-03-10", in(e.item("20.00", "AG"), "DE-BE")), "")
	if j := items(t, guarded)[0]["tax_jurisdiction"]; j != "DE-BE" {
		t.Errorf("an item of a code listing the subdivisions it accepts: got jurisdiction %v, want DE-BE", j)
	}
}

// TestPostJurisdictions: the posting carries each item's jurisdiction to
// its line and posts the code's tax per jurisdiction, the items' shares
// there, so the entry's tax items are per jurisdiction and add up to the
// invoice's tax; a credit memo posts the same with the signs turned.
func TestPostJurisdictions(t *testing.T) {
	e := setup(t)
	for _, c := range []struct {
		name, kind, lines, items string
	}{
		{"an invoice", "invoice", "120000 26965 - -; 400000 -10010 AJ DE-BY; 400000 -3333 AJ DE-BE; 400000 -1005 AJ DE-BY; 400000 -10000 A1 -; " +
			"230000 -550 AJ DE-BY; 230000 -167 AJ DE-BE; 230000 -1900 A1 -",
			"A1 - -10000 -1900; AJ DE-BE -3333 -167; AJ DE-BY -11015 -550"},
		{"a credit memo", "credit_memo", "120000 -26965 - -; 400000 10010 AJ DE-BY; 400000 3333 AJ DE-BE; 400000 1005 AJ DE-BY; 400000 10000 A1 -; " +
			"230000 550 AJ DE-BY; 230000 167 AJ DE-BE; 230000 1900 A1 -",
			"A1 - 10000 1900; AJ DE-BE 3333 167; AJ DE-BY 11015 550"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(e.emirates(), `"currency": "EUR", "items"`, fmt.Sprintf(`"kind": %q, "currency": "EUR", "items"`, c.kind), 1)
			got := e.post(t, e.entered(t, body), "2026-03-12")
			if lines := e.jurisdictionLines(t, got["journal_entry_id"]); lines != c.lines {
				t.Errorf("lines: got %s, want %s", lines, c.lines)
			}
			if its := e.taxItems(t, got["journal_entry_id"]); its != c.items {
				t.Errorf("tax items: got %s, want %s", its, c.items)
			}
		})
	}
}

// TestPostJurisdictionsPerLine: with tax rounded per line (the company's
// tax settings), each item's tax is its own, 5.01, 1.67 and 0.50, and a
// jurisdiction's tax their sum there, which the ledger accepts.
func TestPostJurisdictionsPerLine(t *testing.T) {
	e := setupRounding(t, "line")
	got := e.post(t, e.entered(t, e.emirates()), "2026-03-12")
	want := "120000 26966 - -; 400000 -10010 AJ DE-BY; 400000 -3333 AJ DE-BE; 400000 -1005 AJ DE-BY; 400000 -10000 A1 -; " +
		"230000 -551 AJ DE-BY; 230000 -167 AJ DE-BE; 230000 -1900 A1 -"
	if lines := e.jurisdictionLines(t, got["journal_entry_id"]); lines != want {
		t.Errorf("lines: got %s, want %s", lines, want)
	}
}

// TestPostJurisdictionNoLongerLevied: a draft whose item names a
// jurisdiction its code no longer accepts when it posts (the
// configuration's list changed since) is refused, naming the item's
// field, and stays a draft: it is entered again.
func TestPostJurisdictionNoLongerLevied(t *testing.T) {
	e := setup(t)
	id := e.entered(t, e.invoice(e.customer, "2026-03-10", in(e.item("20.00", "AG"), "DE-BE")))
	doc := fixture.Document(t)
	outputCodes("")(doc)
	ag := doc.Company("2000").TaxCodes[slicesIndex(doc.Company("2000").TaxCodes, "AG")]
	ag.Components[0].Subdivisions = []string{"DE-BY"}
	fixture.TaxCodes(t, e.Tenant, doc, ag)
	before := e.entries(t)
	p := e.Problem(422, "receivables.tax_jurisdiction_not_levied", "POST", "/v1/customer-invoices/"+id+":post", `{"posting_date": "2026-03-12"}`, `"1"`)
	if f := field(p); f != "items[0].tax_jurisdiction" {
		t.Errorf("field: got %v, want items[0].tax_jurisdiction", f)
	}
	d, _ := p["detail"].(string)
	if !strings.Contains(d, "DE-BE") {
		t.Errorf("detail: got %q, want it naming the item's jurisdiction DE-BE", d)
	}
	if !strings.Contains(d, "it levies DE-BY") {
		t.Errorf("detail: got %q, want it naming what the code levies, DE-BY", d)
	}
	if n := e.entries(t); n != before {
		t.Errorf("journal entries: got %d more, want none", n-before)
	}
	var status string
	pgtest.Scan(t, `SELECT status FROM receivables.customer_invoice WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, id}, &status)
	if status != "draft" {
		t.Errorf("the invoice: got %s, want it left a draft", status)
	}
}
