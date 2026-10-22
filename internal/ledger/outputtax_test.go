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
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// outputCodes gives company 2000 an output tax account 240000 and the
// output codes of the worked cases: S5, the UAE's standard rate, levied in
// the emirate each line names (VAT 201 boxes 1a-1g); HON, Ontario's HST;
// Z0 zero-rated and E0 exempt, recorded at rate 0.
func outputCodes(t *testing.T, e env) {
	t.Helper()
	doc := document(t)
	co := doc.Company("2000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
	rate := func(pct string) []config.TaxRate {
		return []config.TaxRate{{RatePercent: pct, ValidFrom: "2026-01-01"}}
	}
	out := func(code, kind, pct, jurisdiction string, onLine bool) config.TaxCode {
		return config.TaxCode{Code: code, Description: code, Direction: config.TaxOutput, Kind: kind, Components: []config.TaxComponent{
			{Key: "vat", AccountKey: config.KeyOutput, Account: "240000", Jurisdiction: jurisdiction, JurisdictionOnLine: onLine, Rates: rate(pct)}}}
	}
	fixture.TaxCodes(t, e.ten, doc, out("S5", config.TaxStandard, "5", "AE", true), out("HON", config.TaxStandard, "13", "CA-ON", false),
		out("Z0", config.TaxZero, "0", "", false), out("E0", config.TaxExempt, "0", "", false))
}

// saleLine is a line of a sales entry body: account, amount, code and
// jurisdiction (either may be empty).
type saleLine struct{ account, amount, code, jurisdiction string }

// saleEntry is a manual entry body of company 2000 in EUR.
func saleEntry(t *testing.T, e env, lines ...saleLine) string {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		extra := ""
		if l.code != "" {
			extra = fmt.Sprintf(`, "tax_code": %q`, l.code)
		}
		if l.jurisdiction != "" {
			extra += fmt.Sprintf(`, "tax_jurisdiction": %q`, l.jurisdiction)
		}
		out[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, e.account(t, "2000", l.account), l.amount, extra)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-10", "posting_date": "2026-03-10", "currency": "EUR", "lines": [%s]}`,
		e.de, strings.Join(out, ", "))
}

// salesItems renders an entry's items with their jurisdictions.
func salesItems(t *testing.T, entry string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(string_agg(code || '/' || coalesce(jurisdiction, '-') || ' ' || base_tc || ' ' || tax_tc || ' at ' || trim_scale(rate_pct),
		'; ' ORDER BY item_no), '') FROM ledger.tax_item WHERE journal_entry_id = $1`, []any{entry}, &got)
	return got
}

// TestOutputTax: a billing system's sales post as journal entries, one per
// sales invoice or a summary per code and jurisdiction (P2, T61f); each
// output code's tax lines are the tax on its base lines within one minor
// unit; the code's items are per jurisdiction; zero-rated and exempt bases
// are items at rate 0; a tax line alone is an adjustment.
func TestOutputTax(t *testing.T) {
	e := setup(t)
	outputCodes(t, e)
	for name, c := range map[string]struct {
		body, items string
	}{
		"a sales invoice in Dubai": {saleEntry(t, e, saleLine{"100000", "105.00", "", ""}, saleLine{"400000", "-100.00", "S5", "AE-DU"},
			saleLine{"240000", "-5.00", "S5", "AE-DU"}), "S5/AE-DU -10000 -500 at 5"},
		"a summary by emirate": {saleEntry(t, e, saleLine{"100000", "3150.00", "", ""}, saleLine{"400000", "-1000.00", "S5", "AE-DU"},
			saleLine{"240000", "-50.00", "S5", "AE-DU"}, saleLine{"400000", "-2000.00", "S5", "AE-AZ"}, saleLine{"240000", "-100.00", "S5", "AE-AZ"}),
			"S5/AE-AZ -200000 -10000 at 5; S5/AE-DU -100000 -5000 at 5"},
		"Ontario HST": {saleEntry(t, e, saleLine{"100000", "113.00", "", ""}, saleLine{"400000", "-100.00", "HON", ""}, saleLine{"240000", "-13.00", "HON", ""}),
			"HON/CA-ON -10000 -1300 at 13"},
		"zero-rated and exempt": {saleEntry(t, e, saleLine{"100000", "150.00", "", ""}, saleLine{"400000", "-100.00", "Z0", ""},
			saleLine{"400000", "-50.00", "E0", ""}), "E0/- -5000 0 at 0; Z0/- -10000 0 at 0"},
		// 5% of 100.10 is 5.005, 5.01 rounded; 5.00 is within a unit.
		"within a minor unit": {saleEntry(t, e, saleLine{"100000", "105.10", "", ""}, saleLine{"400000", "-100.10", "S5", "AE-DU"},
			saleLine{"240000", "-5.00", "S5", "AE-DU"}), "S5/AE-DU -10010 -500 at 5"},
		// A summary of four invoices of 100.10, each taxed 5.01: 20.04,
		// where 5% of the sum is 20.02; a unit per base line allows it.
		"a summary of rounded invoices": {saleEntry(t, e, saleLine{"100000", "420.44", "", ""}, saleLine{"400000", "-100.10", "S5", "AE-DU"},
			saleLine{"400000", "-100.10", "S5", "AE-DU"}, saleLine{"400000", "-100.10", "S5", "AE-DU"}, saleLine{"400000", "-100.10", "S5", "AE-DU"},
			saleLine{"240000", "-20.04", "S5", "AE-DU"}), "S5/AE-DU -40040 -2004 at 5"},
		"an adjustment of tax alone": {saleEntry(t, e, saleLine{"240000", "5.00", "S5", "AE-DU"}, saleLine{"100000", "-5.00", "", ""}),
			"S5/AE-DU 0 500 at 5"},
	} {
		t.Run(name, func(t *testing.T) {
			je, tag := e.in(t).Must(201, "POST", "/v1/journal-entries", c.body, "")
			id := apitest.String(t, je, "id")
			if got := salesItems(t, id); got != c.items {
				t.Errorf("items: got %s, want %s", got, c.items)
			}
			rev, _ := e.in(t).Must(200, "POST", "/v1/journal-entries/"+id+":reverse", "", tag)
			if got := rev["lines"].([]any)[1].(map[string]any)["tax_jurisdiction"]; got != je["lines"].([]any)[1].(map[string]any)["tax_jurisdiction"] {
				t.Errorf("reversal's jurisdiction: got %v, want the original's", got)
			}
		})
	}
}

// TestOutputTaxRefused: tax lines that are not the tax on the base lines,
// a missing or foreign jurisdiction, and amounts out of range are refused.
func TestOutputTaxRefused(t *testing.T) {
	e := setup(t)
	outputCodes(t, e)
	huge := "50000000000000000.00"
	for name, c := range map[string]struct{ body, code string }{
		"the wrong tax": {saleEntry(t, e, saleLine{"100000", "106.00", "", ""}, saleLine{"400000", "-100.00", "S5", "AE-DU"},
			saleLine{"240000", "-6.00", "S5", "AE-DU"}), "ledger.output_tax_mismatch"},
		"beyond a unit per base line": {saleEntry(t, e, saleLine{"100000", "102.03", "", ""}, saleLine{"400000", "-50.00", "S5", "AE-DU"},
			saleLine{"400000", "-47.00", "S5", "AE-DU"}, saleLine{"240000", "-5.03", "S5", "AE-DU"}), "ledger.output_tax_mismatch"},
		"no tax line": {saleEntry(t, e, saleLine{"100000", "100.00", "", ""}, saleLine{"400000", "-100.00", "HON", ""}), "ledger.output_tax_mismatch"},
		"no jurisdiction": {saleEntry(t, e, saleLine{"100000", "105.00", "", ""}, saleLine{"400000", "-100.00", "S5", ""},
			saleLine{"240000", "-5.00", "S5", ""}), "ledger.jurisdiction_required"},
		"another country's": {saleEntry(t, e, saleLine{"100000", "105.00", "", ""}, saleLine{"400000", "-100.00", "S5", "CA-ON"},
			saleLine{"240000", "-5.00", "S5", "CA-ON"}), "ledger.jurisdiction_not_levied"},
		"a code of its own jurisdiction": {saleEntry(t, e, saleLine{"100000", "113.00", "", ""}, saleLine{"400000", "-100.00", "HON", "CA-ON"},
			saleLine{"240000", "-13.00", "HON", "CA-ON"}), "ledger.jurisdiction_not_levied"},
		"no tax code": {saleEntry(t, e, saleLine{"100000", "100.00", "", "AE-DU"}, saleLine{"400000", "-100.00", "", ""}), "ledger.jurisdiction_not_levied"},
		"bases out of range": {saleEntry(t, e, saleLine{"400000", "-" + huge, "Z0", ""}, saleLine{"100000", huge, "", ""}, saleLine{"400000", "-" + huge, "Z0", ""},
			saleLine{"100000", huge, "", ""}), "ledger.amount_too_large"},
		"tax lines out of range": {saleEntry(t, e, saleLine{"240000", "-" + huge, "HON", ""}, saleLine{"100000", huge, "", ""},
			saleLine{"240000", "-" + huge, "HON", ""}, saleLine{"100000", huge, "", ""}), "ledger.amount_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(422, c.code, "POST", "/v1/journal-entries", c.body, "")
		})
	}
}

// TestParkedJurisdiction: a parked entry keeps its lines' jurisdictions.
func TestParkedJurisdiction(t *testing.T) {
	e := setup(t)
	outputCodes(t, e)
	parked, _ := e.Must(201, "POST", "/v1/parked-journal-entries", saleEntry(t, e, saleLine{"100000", "105.00", "", ""},
		saleLine{"400000", "-100.00", "S5", "AE-DU"}, saleLine{"240000", "-5.00", "S5", "AE-DU"}), "")
	changed, _ := e.Must(200, "PATCH", "/v1/parked-journal-entries/"+apitest.String(t, parked, "id"), `{"header_text": "Invoice 4711"}`, `"1"`)
	for name, pe := range map[string]map[string]any{"parked": parked, "changed": changed} {
		if got := pe["lines"].([]any)[1].(map[string]any)["tax_jurisdiction"]; got != "AE-DU" {
			t.Errorf("%s entry's jurisdiction: got %v, want AE-DU", name, got)
		}
	}
}

// TestOutputTaxFailures fails each statement of posting a sales entry.
func TestOutputTaxFailures(t *testing.T) {
	e := setup(t)
	outputCodes(t, e)
	h := endpoints()
	line := func(account, amount, code string) gen.JournalLineInput {
		l := gen.JournalLineInput{AccountID: uuid.MustParse(e.account(t, "2000", account)), Amount: gen.Money{Amount: amount, Currency: "EUR"}}
		if code != "" {
			l.TaxCode, l.TaxJurisdiction = gen.NewOptString(code), gen.NewOptString("AE-DU")
		}
		return l
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "post a sales entry", func(ctx context.Context) error {
		_, err := h.PostJournalEntry(ctx, &gen.JournalEntryCreate{LegalEntityID: e.de, DocumentDate: date("2026-03-10"), PostingDate: date("2026-03-10"),
			Currency: "EUR", Lines: []gen.JournalLineInput{line("100000", "105.00", ""), line("400000", "-100.00", "S5"), line("240000", "-5.00", "S5")}},
			gen.PostJournalEntryParams{})
		return err
	})
}
