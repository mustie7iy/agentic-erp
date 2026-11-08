package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/tax"
	"github.com/mustie7iy/agentic-erp/packs"
)

// TestUAEPackTaxCodes activates UAE pack 2026.1's VAT codes (T62f) in
// company 2000, with the pack's VAT accounts 1300 and 2300, and calculates
// AED 1,000.00 with each on 2026-06-01, as payables and the ledger do
// (Accounts.TaxCode, then the calculator): the golden cases of
// packs/ae/TAX-PENDING-T62f.md. A standard purchase recovers its 50.00;
// a blocked one recovers nothing; the reverse charge, the domestic reverse
// charge and import VAT owe 50.00 and recover it, netting to zero (ADR-0049
// §9); a standard sale owes 50.00; zero-rated, exempt and out-of-scope
// supplies are recorded at 0. No rate is in force before the pack's
// 2026-01-01.
func TestUAEPackTaxCodes(t *testing.T) {
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.1")
	if ae == nil {
		t.Fatal("Get(AE, 2026.1): got nil, want the UAE pack")
	}
	e := setup(t)
	doc := document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "1300", Name: "VAT recoverable", Type: config.Asset},
		config.Account{Number: "2300", Name: "VAT payable", Type: config.Liability})
	de.TaxCodes = append(de.TaxCodes, ae.Tax.TaxCodes...)
	if err := e.applyTax(t, doc); err != nil {
		t.Fatalf("activate the pack's tax codes: %v", err)
	}
	// An owed part is wholly on its account, which the calculator reports
	// as its recoverable part (ADR-0049 §8 As built).
	want := map[string]tax.Amounts{
		"V5 vat":      {Base: 100000, Tax: 5000, Recoverable: 5000},
		"VB5 vat":     {Base: 100000, Tax: 5000, NonRecoverable: 5000},
		"RC5 vat_due": {Base: 100000, Tax: -5000, Recoverable: -5000},
		"RC5 vat":     {Base: 100000, Tax: 5000, Recoverable: 5000},
		"DR5 vat_due": {Base: 100000, Tax: -5000, Recoverable: -5000},
		"DR5 vat":     {Base: 100000, Tax: 5000, Recoverable: 5000},
		"IM5 vat_due": {Base: 100000, Tax: -5000, Recoverable: -5000},
		"IM5 vat":     {Base: 100000, Tax: 5000, Recoverable: 5000},
		"S5 vat":      {Base: 100000, Tax: 5000, Recoverable: 5000},
		"Z0 vat":      {Base: 100000},
		"E0 vat":      {Base: 100000},
		"OS vat":      {Base: 100000},
	}
	res := e.packTax(t, "2026-06-01", "AED", "V5", "VB5", "RC5", "DR5", "IM5", "S5", "Z0", "E0", "OS")
	got := map[string]tax.Amounts{}
	for _, it := range res.Items {
		got[it.Code+" "+it.Key] = it.Amounts
	}
	if len(got) != len(want) {
		t.Errorf("items: got %d %v, want %d", len(got), got, len(want))
	}
	for item, w := range want {
		t.Run(item, func(t *testing.T) {
			if got[item] != w {
				t.Errorf("got %+v, want %+v", got[item], w)
			}
		})
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, ok, err := ledger.Accounts{}.TaxCode(ctx, e.de, "V5", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
		if err != nil {
			return fmt.Errorf("V5 on 2025-12-31: %w", err)
		}
		if ok {
			t.Error("V5 on 2025-12-31: got a rate, want none before the pack's 2026-01-01")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
