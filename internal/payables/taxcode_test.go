package payables_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// taxItems renders a posted invoice's entry's tax items, as code/component
// base tax recoverable at the rate on the tax date.
func taxItems(t *testing.T, entry any) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(string_agg(code || '/' || component || ' ' || base_tc || ' ' || tax_tc || ' ' || recoverable_tc || ' at ' || trim_scale(rate_pct)
			|| ' on ' || tax_date, '; ' ORDER BY item_no), '') FROM ledger.tax_item WHERE journal_entry_id = $1`, []any{entry}, &got)
	return got
}

// TestTaxCodeChangedBeforePosting: an invoice's tax is calculated when it
// posts (ADR-0049 §4): entered with a one-rate code that gains a
// non-deductible component before it posts, its stated tax no longer
// matches (422 payables.tax_mismatch, decision P5, naming the code's
// figure in its detail and its fields); corrected, it posts the GST on the tax account and the PST on
// the item's line, with an item per component.
func TestTaxCodeChangedBeforePosting(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	gained := twoParts
	gained.Code = "V1"
	gained.Components = append([]config.TaxComponent{{Key: config.LegacyComponent, AccountKey: config.KeyInput, Account: "140000",
		Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2026-01-01"}}}}, twoParts.Components[1])
	e.TaxCodes(t, gained)
	p := e.Problem(422, "payables.tax_mismatch", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
	if detail := fmt.Sprint(p["detail"]); !strings.Contains(detail, "26.00 (V1 26.00)") {
		t.Errorf("detail: got %s, want V1's 26.00 named", detail)
	}
	// The same figures as fields, for a client's own words (ADR-0052 D3).
	figures, err := json.Marshal(p["calculated_tax"])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(figures), `{"calculated":{"amount":"26.00","currency":"EUR"},"codes":[{"tax":{"amount":"26.00","currency":"EUR"},"tax_code":"V1"}],`+
		`"stated":{"amount":"19.00","currency":"EUR"}}`; got != want {
		t.Errorf("calculated_tax:\ngot  %s\nwant %s", got, want)
	}
	e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"gross_amount": {"amount": "126.00", "currency": "EUR"}, "tax_amount": {"amount": "26.00", "currency": "EUR"}}`,
		e.tag(t, id(inv)))
	posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-21"}`)
	want := []line{{e.expense.String(), "107.00", nil}, {e.tax.String(), "19.00", nil}, {e.ap.String(), "-126.00", nil}}
	if got := e.entryLines(t, posted["journal_entry_id"]); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", got, want)
	}
	if got, want := taxItems(t, posted["journal_entry_id"]), "V1/pst 10000 700 0 at 7 on 2026-03-10; V1/tax 10000 1900 1900 at 19 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}
