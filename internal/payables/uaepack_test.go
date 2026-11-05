package payables_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/packs"
)

// TestUAEPackDocumentaryWarning: with the tax settings of UAE pack 2026.1
// (T62f, ADR-0049 §6 As built) in a company of the UAE, an invoice above
// 10,000 of the company's currency from a supplier without a UAE tax
// identifier posts with the warning that its TRN is missing, the tier
// exclusive and pending sign-off; one of exactly 10,000, one below it, and
// one from a supplier holding a UAE identifier post without one.
func TestUAEPackDocumentaryWarning(t *testing.T) {
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.1")
	if ae == nil {
		t.Fatal("Get(AE, 2026.1): got nil, want the UAE pack")
	}
	e := setup(t)
	// Company 2000's legal entity is moved to the UAE, so a scheme named
	// after it is its country's (AE_...); its currency stays EUR, which the
	// tier's whole units are read in.
	pgtest.Exec(t, `UPDATE tenancy.legal_entity SET country_code = 'AE', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, e.DE)
	e.withTaxSettings(t, ae.Tax.Settings)
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, 'AE_TRN', '\x00', repeat('c', 64), '0003', $3, $3)`, e.Tenant.ID, e.Vendor, e.Tenant.Human.ID)
	cases := []struct {
		name, gross, tax, net string
		withTRN               bool
		want                  []string
	}{
		{"above, without a TRN", "11900.00", "1900.00", "10000.00", false, []string{"supplier_tax_id_missing 10000.00 EUR false true"}},
		{"exactly the threshold", "10000.00", "1596.64", "8403.36", false, nil},
		{"below", "5950.00", "950.00", "5000.00", false, nil},
		{"above, with a TRN", "11900.00", "1900.00", "10000.00", true, nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supplier := e.Supplier
			if tc.withTRN {
				supplier = e.Vendor
			}
			inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(supplier, fmt.Sprintf("AE-%d", i), tc.gross, tc.tax, accountItem(e.expense, tc.net)), "")
			posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-21"}`)
			if got := warnings(posted); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("posted: got %v, want %v", got, tc.want)
			}
		})
	}
}
