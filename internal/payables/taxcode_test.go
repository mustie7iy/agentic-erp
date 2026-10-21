package payables_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestTaxCodeChangedBeforePosting: an invoice entered with a one-rate code
// is refused when it posts after the code gained a component one tax line
// cannot post (422 payables.tax_code_components, until T61d).
func TestTaxCodeChangedBeforePosting(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "P-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	gained := twoParts
	gained.Code = "V1"
	gained.Components = append([]config.TaxComponent{{Key: config.LegacyComponent, AccountKey: config.KeyInput, Account: "140000",
		Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2026-01-01"}}}}, twoParts.Components[1])
	e.TaxCodes(t, gained)
	e.Problem(422, "payables.tax_code_components", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
}
