package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/tax"
	"github.com/mustie7iy/agentic-erp/packs"
)

// TestCanadaPackTaxCodes activates Canada pack 2026.4's tax codes (T62g)
// in company 2000, with the pack's GST/HST and QST receivable accounts:
// the ledger sets up every dated rate, past and future, and a purchase of
// 1,000.00 with HON and one with QST on 2026-06-01 are taxed 130.00 HST
// and 99.75 QST, wholly recoverable, as payables calculates an invoice's
// tax (Accounts.TaxCode, then the calculator); BC PST is not recoverable.
func TestCanadaPackTaxCodes(t *testing.T) {
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.4")
	if ca == nil {
		t.Fatal("Get(CA, 2026.4): got nil, want the Canada pack with tax codes")
	}
	e := setup(t)
	doc := document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "1300", Name: "GST/HST receivable", Type: config.Asset},
		config.Account{Number: "1310", Name: "QST receivable", Type: config.Asset})
	de.TaxCodes = append(de.TaxCodes, ca.Tax.TaxCodes...)
	if err := e.applyTax(t, doc); err != nil {
		t.Fatalf("activate the pack's tax codes: %v", err)
	}
	if got, want := e.taxRates(t, "HNS", "HON", "QST"), "HNS hst 15 2013-04-01 2025-04-01, HNS hst 14 2025-04-01 open, HON hst 13 2013-04-01 open, QST qst 9.975 2026-01-01 open"; got != want {
		t.Errorf("rates: got %s, want %s", got, want)
	}
	res := e.canadaTax(t, "2026-06-01", "HON", "QST", "PBC")
	want := map[string]tax.Amounts{
		"HON hst":    {Base: 100000, Tax: 13000, Recoverable: 13000},
		"QST qst":    {Base: 100000, Tax: 9975, Recoverable: 9975},
		"PBC pst_bc": {Base: 100000, Tax: 7000, NonRecoverable: 7000},
	}
	for _, it := range res.Items {
		if got := it.Amounts; got != want[it.Code+" "+it.Key] {
			t.Errorf("%s %s: got %+v, want %+v", it.Code, it.Key, got, want[it.Code+" "+it.Key])
		}
	}
	for day, want := range map[string]money.Minor{"2025-03-31": 15000, "2025-04-01": 14000} {
		if got := e.canadaTax(t, day, "HNS").Items[0].Tax; got != want {
			t.Errorf("HNS on %s: got %d, want %d", day, got, want)
		}
	}
}

// canadaTax is the tax of a purchase of 1,000.00 per code on the day, with
// the codes as company 2000's ledger answers them then.
func (e env) canadaTax(t *testing.T, day string, codes ...string) tax.Result {
	t.Helper()
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		t.Fatal(err)
	}
	doc := tax.Document{TaxDate: d, Currency: "CAD", Codes: map[string]tax.Code{}}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		for _, code := range codes {
			tc, ok, err := ledger.Accounts{}.TaxCode(ctx, e.de, code, d)
			if err != nil || !ok {
				t.Fatalf("%s on %s: got %v %v, want found", code, day, ok, err)
			}
			doc.Codes[code] = tc.Calculable()
			doc.Lines = append(doc.Lines, tax.Line{Amount: 100000, Code: code})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := tax.Calculate(doc)
	if err != nil {
		t.Fatalf("calculate on %s: %v", day, err)
	}
	if len(res.Items) != len(codes) {
		t.Fatalf("items on %s: got %d, want one per code", day, len(res.Items))
	}
	return res
}
