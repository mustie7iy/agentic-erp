package sales_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withA2 is outputCodes with a further plain output code A2, levying no
// subdivision, which a test changes on its own.
func withA2(doc *config.Config) {
	outputCodes(doc)
	de := doc.Company("2000")
	a1 := de.TaxCodes[slices.IndexFunc(de.TaxCodes, func(c config.TaxCode) bool { return c.Code == "A1" })]
	a2 := a1
	a2.Code, a2.Description = "A2", "Output tax A2"
	a2.Components = slices.Clone(a1.Components)
	de.TaxCodes = append(de.TaxCodes, a2)
}

// TestOrderFromQuotationRechecked: an order made from a quotation checks
// its items again on the order's date, as an order entered directly is
// checked (ADR-0067 §4), the configuration having changed since the
// quotation's: an item whose account can no longer be posted in the
// company, whose tax code has no rate in force that day, whose code no
// longer lists its subdivision, or whose code levies its lines'
// subdivision since and which names none, refuses the order, naming
// quotation_id and the item; nothing is ordered, and the quotation stays
// accepted for an order once the configuration allows it.
func TestOrderFromQuotationRechecked(t *testing.T) {
	e := setupWith(t, withA2)
	account := e.accepted(t, e.customer, e.item(line{}), e.item(line{account: e.other}))
	rate := e.accepted(t, e.customer, e.item(line{code: "A1"}))
	listed := e.accepted(t, e.customer, e.item(line{code: "AL", jurisdiction: "DE-BE"}))
	unnamed := e.accepted(t, e.customer, e.item(line{}), e.item(line{code: "A2"}))
	quoted, _ := e.Must(200, "GET", "/v1/quotations/"+rate, "", "")
	day, _ := quoted["quotation_date"].(string)
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND account_id = $2`,
		e.Tenant.ID, e.other)
	doc := fixture.Document(t)
	withA2(doc)
	codes := doc.Company("2000").TaxCodes
	code := func(name string) config.TaxCode {
		c := codes[slices.IndexFunc(codes, func(c config.TaxCode) bool { return c.Code == name })]
		c.Components = slices.Clone(c.Components)
		return c
	}
	a1, al, a2 := code("A1"), code("AL"), code("A2")
	a1.Components[0].Rates = []config.TaxRate{{RatePercent: "19", ValidFrom: "2099-01-01"}}
	al.Components[0].Subdivisions = []string{"DE-BY"}
	a2.Components[0].Jurisdiction, a2.Components[0].JurisdictionOnLine = "DE", true
	fixture.TaxCodes(t, e.Tenant, doc, a1, al, a2)
	cases := []struct{ name, quotation, detail string }{
		{"an account that can no longer be posted", account, "Item 2 of quotation QT"},
		{"a tax code without a rate on the order's date", rate, "Item 1 of quotation QT"},
		{"a subdivision no longer listed", listed, "Item 1 of quotation QT"},
		{"a code levying its lines' subdivision since", unnamed, "Item 2 of quotation QT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, "sales.quotation_item_not_orderable", "POST", "/v1/sales-orders", fromQuotation(c.quotation), "")
			if f := field(p); f != "quotation_id" {
				t.Errorf("field: got %v, want quotation_id", f)
			}
			d, _ := p["detail"].(string)
			if !strings.Contains(d, c.detail) {
				t.Errorf("detail: got %q, want it to contain %q", d, c.detail)
			}
			if want := "cannot be ordered on " + day + ": "; !strings.Contains(d, want) {
				t.Errorf("detail: got %q, want it to contain %q", d, want)
			}
			got, _ := e.In(t).Must(200, "GET", "/v1/quotations/"+c.quotation, "", "")
			if got["status"] != "accepted" {
				t.Errorf("quotation status: got %v, want accepted", got["status"])
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.sales_order WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("sales orders: got %d, want none", n)
	}
	t.Run("ordered once the configuration allows it", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = false, version = version + 1 WHERE tenant_id = $1 AND account_id = $2`,
			e.Tenant.ID, e.other)
		e.In(t).Must(201, "POST", "/v1/sales-orders", fromQuotation(account), "")
	})
}
