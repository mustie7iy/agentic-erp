package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// importCodes gives company 2000 an output tax account 240000, a customs
// clearing account 219500 and the import codes of the worked cases: IM5,
// the UAE's import VAT accounted for in the return (VAT 201 box 6, its
// recovery box 10), self-assessed on 240000 and recovered on 140000; IC5,
// Canada's GST paid to the CBSA on an import, an input tax credit (GST34
// line 106); IH5, an import whose VAT is half recovered.
func importCodes(t *testing.T, e env) {
	t.Helper()
	doc := document(t)
	co := doc.Company("2000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability},
		config.Account{Number: "219500", Name: "Customs clearing", Type: config.Liability})
	five := []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}
	fixture.TaxCodes(t, e.ten, doc,
		config.TaxCode{Code: "IM5", Description: "UAE import VAT", Kind: config.TaxImport, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: five},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", Rates: five}}},
		config.TaxCode{Code: "IC5", Description: "GST on imports (CBSA)", Kind: config.TaxImport, Components: []config.TaxComponent{
			{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: five}}},
		config.TaxCode{Code: "IH5", Description: "Import VAT, half recovered", Kind: config.TaxImport, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: five},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", RecoverablePercent: "50", Rates: five}}})
}

// importLine is a line of an import entry body; base, when set, is its
// declared tax base.
type importLine struct{ account, amount, code, base string }

// importEntry is a manual entry body of company 2000 in the currency.
func importEntry(t *testing.T, e env, currency string, lines ...importLine) string {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		extra := ""
		if l.code != "" {
			extra = fmt.Sprintf(`, "tax_code": %q`, l.code)
		}
		if l.base != "" {
			extra += fmt.Sprintf(`, "tax_base": {"amount": %q, "currency": %q}`, l.base, currency)
		}
		out[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": %q}%s}`, e.account(t, "2000", l.account), l.amount, currency, extra)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-10", "posting_date": "2026-03-10", "currency": %q, "lines": [%s]}`,
		e.de, currency, strings.Join(out, ", "))
}

// TestImportFromCustomsDeclaration: an import's tax is posted from the
// customs declaration as tax lines alone, each naming the declared value
// as its base (ADR-0049 §9, T61e); the component's item takes it, in local
// currency at the entry's rate; the API shows it on the line, and a
// reversal negates it.
func TestImportFromCustomsDeclaration(t *testing.T) {
	e := setup(t)
	importCodes(t, e)
	settleRate(t, e, "AED", "2026-01-01", "0.25")
	settleRate(t, e, "CAD", "2026-01-01", "0.7")
	for name, c := range map[string]struct {
		body, items string
	}{
		// AED 10,000.00 of goods cleared by a registrant who accounts for
		// the import VAT in the return: box 6 owes 500.00, box 10 recovers it.
		"UAE box 6": {importEntry(t, e, "AED", importLine{"140000", "500.00", "IM5", "10000.00"}, importLine{"240000", "-500.00", "IM5", "10000.00"}),
			"IM5/claim 2026-03-10 1000000 50000 50000 (250000 12500 12500) at 5; IM5/due 2026-03-10 1000000 -50000 0 (250000 -12500 0) at 5"},
		// CAD 13,000.00 value for duty: the broker paid the CBSA 650.00
		// GST, claimed as an input tax credit (line 106).
		"Canada CBSA": {importEntry(t, e, "CAD", importLine{"140000", "650.00", "IC5", "13000.00"}, importLine{"219500", "-650.00", "", ""}),
			"IC5/gst 2026-03-10 1300000 65000 65000 (910000 45500 45500) at 5"},
		// Half recovered: the recovered part on the input account, the
		// rest an expense without a code.
		"half recovered": {importEntry(t, e, "AED", importLine{"140000", "250.00", "IH5", "10000.00"}, importLine{"520000", "250.00", "", ""},
			importLine{"240000", "-500.00", "IH5", "10000.00"}),
			"IH5/claim 2026-03-10 1000000 25000 25000 (250000 6250 6250) at 5; IH5/due 2026-03-10 1000000 -50000 0 (250000 -12500 0) at 5"},
	} {
		t.Run(name, func(t *testing.T) {
			je, tag := e.in(t).Must(201, "POST", "/v1/journal-entries", c.body, "")
			id := apitest.String(t, je, "id")
			if got := taxItemsOf(t, e, uuid.MustParse(id)); got != c.items {
				t.Errorf("items:\ngot  %s\nwant %s", got, c.items)
			}
			line := je["lines"].([]any)[0].(map[string]any)
			if base, ok := line["tax_base"].(map[string]any); !ok || base["amount"] == nil || line["tax_base_lc"] == nil {
				t.Errorf("first line's declared base: got %v and %v, want both", line["tax_base"], line["tax_base_lc"])
			}
			rev, _ := e.in(t).Must(200, "POST", "/v1/journal-entries/"+id+":reverse", "", tag)
			revLine := rev["lines"].([]any)[0].(map[string]any)["tax_base"].(map[string]any)
			if got := revLine["amount"]; got != "-"+line["tax_base"].(map[string]any)["amount"].(string) {
				t.Errorf("reversal's declared base: got %v, want the original's negated", got)
			}
		})
	}
}

// TestTaxBaseRefused: a declared base is only on a tax line of an import
// code, never zero, in the entry's currency, and its line's amount is the
// tax on it.
func TestTaxBaseRefused(t *testing.T) {
	e := setup(t)
	importCodes(t, e)
	for name, c := range map[string]struct {
		body, code string
		status     int
	}{
		"a code not for imports": {importEntry(t, e, "EUR", importLine{"140000", "19.00", "V1", "100.00"}, importLine{"100000", "-19.00", "", ""}),
			"ledger.tax_base_not_import", 422},
		"no tax code": {importEntry(t, e, "EUR", importLine{"140000", "19.00", "", "100.00"}, importLine{"100000", "-19.00", "", ""}),
			"ledger.tax_base_not_import", 422},
		"the base line": {importEntry(t, e, "EUR", importLine{"520000", "100.00", "IC5", "100.00"}, importLine{"100000", "-100.00", "", ""}),
			"ledger.tax_base_not_import", 422},
		"the wrong tax": {importEntry(t, e, "EUR", importLine{"140000", "6.00", "IC5", "100.00"}, importLine{"219500", "-6.00", "", ""}),
			"ledger.tax_base_mismatch", 422},
		"a zero base": {importEntry(t, e, "EUR", importLine{"140000", "5.00", "IC5", "0.00"}, importLine{"219500", "-5.00", "", ""}),
			"ledger.zero_tax_base", 422},
		"not an amount": {importEntry(t, e, "EUR", importLine{"140000", "5.00", "IC5", "1.234"}, importLine{"219500", "-5.00", "", ""}),
			"request.invalid_amount", 400},
		"another currency": {strings.Replace(importEntry(t, e, "EUR", importLine{"140000", "5.00", "IC5", "100.00"}, importLine{"219500", "-5.00", "", ""}),
			`"amount": "100.00", "currency": "EUR"`, `"amount": "100.00", "currency": "USD"`, 1), "ledger.currency_mismatch", 422},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "POST", "/v1/journal-entries", c.body, "")
		})
	}
}

// TestParkedTaxBase: a parked entry keeps its lines' declared bases, shown
// on its lines and kept by a change that leaves the lines.
func TestParkedTaxBase(t *testing.T) {
	e := setup(t)
	importCodes(t, e)
	parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", importEntry(t, e, "EUR", importLine{"140000", "5.00", "IC5", "100.00"},
		importLine{"219500", "-5.00", "", ""}), "")
	path := "/v1/parked-journal-entries/" + apitest.String(t, parked, "id")
	changed, _ := e.Must(200, "PATCH", path, `{"header_text": "Customs entry 4711"}`, `"1"`)
	for name, pe := range map[string]map[string]any{"parked": parked, "changed": changed} {
		base, ok := pe["lines"].([]any)[0].(map[string]any)["tax_base"].(map[string]any)
		if !ok || base["amount"] != "100.00" {
			t.Errorf("%s entry's declared base: got %v, want 100.00", name, base)
		}
	}
}

// TestTaxBaseFailures fails each statement of posting an import entry.
func TestTaxBaseFailures(t *testing.T) {
	e := setup(t)
	importCodes(t, e)
	h := endpoints()
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post an import entry", func(ctx context.Context) error {
		_, err := h.PostJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-10"), PostingDate: date("2026-03-10"),
			Currency: "EUR", Lines: []gen.JournalLineInput{
				{AccountID: uuid.MustParse(e.account(t, "2000", "140000")), Amount: gen.Money{Amount: "5.00", Currency: "EUR"}, TaxCode: gen.NewOptString("IC5"),
					TaxBase: gen.NewOptMoney(gen.Money{Amount: "100.00", Currency: "EUR"})},
				{AccountID: uuid.MustParse(e.account(t, "2000", "219500")), Amount: gen.Money{Amount: "-5.00", Currency: "EUR"}}}}, gen.PostJournalEntryParams{})
		return err
	})
}
