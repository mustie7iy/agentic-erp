package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func rate(pct, from, to string) config.TaxRate {
	return config.TaxRate{RatePercent: pct, ValidFrom: from, ValidTo: to}
}

// inputCodes gives company 2000 the codes the input tax tests use, beside
// V1 (19%): V7 (7%), M5 (19%, half recoverable, as meals), R9 (10% until
// March, 12% from April), P7 (a non-deductible 7% alone) and RC (a 5%
// reverse charge, self-assessed on 240000 and recovered on 140000), QC
// (GST 5% and QST 9.975% on one account), B0 (5% input, none recovered) and RH (RC with half claimed). It
// returns the configuration for a test to change.
func inputCodes(t *testing.T, e env) *config.Config {
	t.Helper()
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
	all := []config.TaxRate{rate("7", "2026-01-01", "")}
	fixture.TaxCodes(t, e.Tenant, doc,
		config.TaxCode{Code: "V7", Description: "Reduced", Components: []config.TaxComponent{{Key: "tax", AccountKey: config.KeyInput, Account: "140000", Rates: all}}},
		config.TaxCode{Code: "M5", Description: "Meals", Components: []config.TaxComponent{{Key: "tax", AccountKey: config.KeyInput, Account: "140000",
			RecoverablePercent: "50", Rates: []config.TaxRate{rate("19", "2026-01-01", "")}}}},
		config.TaxCode{Code: "R9", Description: "Changing", Components: []config.TaxComponent{{Key: "tax", AccountKey: config.KeyInput, Account: "140000",
			Rates: []config.TaxRate{rate("10", "2026-01-01", "2026-03-31"), rate("12", "2026-04-01", "")}}}},
		config.TaxCode{Code: "P7", Description: "BC PST", Components: []config.TaxComponent{{Key: "pst", AccountKey: config.KeyNonDeductible, Rates: all}}},
		config.TaxCode{Code: "RC", Description: "Reverse charge", Kind: config.TaxReverseCharge, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}}}},
		config.TaxCode{Code: "B0", Description: "Blocked", Components: []config.TaxComponent{{Key: "tax", AccountKey: config.KeyInput, Account: "140000",
			RecoverablePercent: "0", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}}}},
		config.TaxCode{Code: "QC", Description: "GST and QST", Components: []config.TaxComponent{
			{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}},
			{Key: "qst", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{rate("9.975", "2026-01-01", "")}}}},
		config.TaxCode{Code: "RH", Description: "Reverse charge, half claimed", Kind: config.TaxReverseCharge, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", RecoverablePercent: "50", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}}}})
	return doc
}

// coded is an item on the expense account with a code.
func coded(e env, amount, code string) string {
	return strings.Replace(accountItem(e.expense, amount), `"V1"`, `"`+code+`"`, 1)
}

// postedEntry posts an invoice on 2026-03-21 and returns its entry's id.
func (e env) postedEntry(t *testing.T, inv map[string]any) any {
	t.Helper()
	return e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-21"}`)["journal_entry_id"]
}

// TestInputTaxPerCode: an invoice's tax is calculated per item code at its
// tax date (ADR-0049 §6, T61d) and posts one tax line per code and
// component; the part of a code's tax not recovered (meals' half)
// goes on the item's line; each component has its item.
func TestInputTaxPerCode(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "PER-CODE", "196.30", "26.30",
		coded(e, "100.00", "V1"), coded(e, "50.00", "V7"), coded(e, "20.00", "M5")), "")
	entry := e.postedEntry(t, inv)
	want := []line{{e.expense.String(), "100.00", nil}, {e.expense.String(), "50.00", nil}, {e.expense.String(), "21.90", nil},
		{e.tax.String(), "19.00", nil}, {e.tax.String(), "3.50", nil}, {e.tax.String(), "1.90", nil}, {e.ap.String(), "-196.30", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got, want := taxItems(t, entry), "V1/tax 10000 1900 1900 at 19 on 2026-03-10; V7/tax 5000 350 350 at 7 on 2026-03-10; M5/tax 2000 380 190 at 19 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}

// TestStatedTaxTolerance: the supplier's stated tax posts when it is
// within one minor unit per code that charges tax (decision P5), its
// difference on the components posting to an account, largest remainder
// first, else on the non-deductible ones' items; beyond it, or with no
// component to take it, the post is refused with 422 payables.tax_mismatch.
func TestStatedTaxTolerance(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	within, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "WITHIN", "196.31", "26.31",
		coded(e, "100.00", "V1"), coded(e, "50.00", "V7"), coded(e, "20.00", "M5")), "")
	entry := e.postedEntry(t, within)
	if got := taxItems(t, entry); !strings.Contains(got, "V1/tax 10000 1901 1901") {
		t.Errorf("items: got %s, want V1 to take the unit", got)
	}
	for name, body := range map[string]string{
		"beyond the tolerance": e.invoice(e.Supplier, "BEYOND", "196.34", "26.34", coded(e, "100.00", "V1"), coded(e, "50.00", "V7"), coded(e, "20.00", "M5")),
		// The supplier charges no reverse charge tax: no code to allow a
		// difference for, nor a component to take it.
		"no component to take it": e.invoice(e.Supplier, "NOWHERE", "101.00", "1.00", coded(e, "100.00", "RC")),
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := e.in(t).Must(201, "POST", "/v1/supplier-invoices", body, "")
			e.in(t).Problem(422, "payables.tax_mismatch", "POST", "/v1/supplier-invoices/"+id(v)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(v)))
		})
	}
}

// TestStatedTaxDifference: the difference to the supplier's tax goes on
// the part of the tax recovered, which may go down to zero; with nothing
// recovered, it is a cost on the item's line.
func TestStatedTaxDifference(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	for name, c := range map[string]struct {
		code, amount, gross, tax string
		lines                    []line
		items                    string
	}{
		"half recovered, less": {"M5", "20.00", "23.79", "3.79", []line{{e.expense.String(), "21.90", nil}, {e.tax.String(), "1.89", nil}, {e.ap.String(), "-23.79", nil}},
			"M5/tax 2000 379 189 at 19 on 2026-03-10"},
		"half recovered, more": {"M5", "20.00", "23.81", "3.81", []line{{e.expense.String(), "21.90", nil}, {e.tax.String(), "1.91", nil}, {e.ap.String(), "-23.81", nil}},
			"M5/tax 2000 381 191 at 19 on 2026-03-10"},
		"none recovered": {"B0", "100.00", "104.99", "4.99", []line{{e.expense.String(), "104.99", nil}, {e.ap.String(), "-104.99", nil}},
			"B0/tax 10000 499 0 at 5 on 2026-03-10"},
	} {
		t.Run(name, func(t *testing.T) {
			inv, _ := e.in(t).Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "DIFF-"+name, c.gross, c.tax, coded(e, c.amount, c.code)), "")
			entry := e.in(t).postedEntry(t, inv)
			if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(c.lines)) {
				t.Errorf("lines: got %v, want %v", sorted(got), sorted(c.lines))
			}
			if got := taxItems(t, entry); got != c.items {
				t.Errorf("items: got %s, want %s", got, c.items)
			}
		})
	}
}

// TestNonDeductibleTax: a non-deductible code's tax (BC PST) is a cost on
// its item's line, and so is the difference to the supplier's stated tax
// when no component puts tax on an account (decision P5).
func TestNonDeductibleTax(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "PST-1", "107.01", "7.01", coded(e, "100.00", "P7")), "")
	entry := e.postedEntry(t, inv)
	want := []line{{e.expense.String(), "107.01", nil}, {e.ap.String(), "-107.01", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got, want := taxItems(t, entry), "P7/pst 10000 701 0 at 7 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}

// TestComponentsOnOneAccount: a code whose components post to one account
// (GST and QST on one input tax account) posts from an invoice, which
// calculates each component's tax: a tax line and an item per component.
func TestComponentsOnOneAccount(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "QC-1", "114.98", "14.98", coded(e, "100.00", "QC")), "")
	entry := e.postedEntry(t, inv)
	want := []line{{e.expense.String(), "100.00", nil}, {e.tax.String(), "5.00", nil}, {e.tax.String(), "9.98", nil}, {e.ap.String(), "-114.98", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got, want := taxItems(t, entry), "QC/gst 10000 500 500 at 5 on 2026-03-10; QC/qst 10000 998 998 at 9.975 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}

// TestReverseChargePartlyClaimed: the part of a self-assessed tax not
// claimed is a cost on the item's line.
func TestReverseChargePartlyClaimed(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "RH-1", "100.00", "0.00", coded(e, "100.00", "RH")), "")
	entry := e.postedEntry(t, inv)
	output := e.Account(t, "2000", "240000").String()
	want := []line{{e.expense.String(), "102.50", nil}, {output, "-5.00", nil}, {e.tax.String(), "2.50", nil}, {e.ap.String(), "-100.00", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got, want := taxItems(t, entry), "RH/claim 10000 500 250 at 5 on 2026-03-10; RH/due 10000 -500 0 at 5 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}

// TestCreditMemoAtTheInvoicesRates: a credit memo naming an invoice is
// calculated at that invoice's tax date and rates (ADR-0049 §4), so after
// a rate change it credits the tax charged; its items carry that date.
func TestCreditMemoAtTheInvoicesRates(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	invoice := strings.Replace(e.invoice(e.Supplier, "R-1", "110.00", "10.00", coded(e, "100.00", "R9")), "2026-03-10", "2026-03-20", 1)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", invoice, "")
	e.postedEntry(t, inv)
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(e.document("credit_memo", "RC-1", "55.00", "5.00",
		fmt.Sprintf(`, "reference_invoice_id": %q`, id(inv)), coded(e, "50.00", "R9")), "2026-03-20", "2026-04-05", 1), "")
	entry := e.act(t, 200, id(memo), "post", `{"posting_date": "2026-04-06"}`)["journal_entry_id"]
	if got, want := taxItems(t, entry), "R9/tax -5000 -500 -500 at 10 on 2026-03-20"; got != want {
		t.Errorf("credit memo's items: got %s, want %s", got, want)
	}
}

// TestTaxDateSetByAPerson: a person's tax date decides the rates and is
// kept on the invoice; cleared, the invoice date does again.
func TestTaxDateSetByAPerson(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	body := strings.Replace(e.invoice(e.Supplier, "DATED", "110.00", "10.00", coded(e, "100.00", "R9")), "2026-03-10", "2026-04-05", 1)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(body, `"invoice_date"`, `"tax_date": "2026-03-20", "invoice_date"`, 1), "")
	if inv["tax_date"] != "2026-03-20" {
		t.Errorf("tax date: got %v, want 2026-03-20", inv["tax_date"])
	}
	cleared, _ := e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"tax_date": null}`, e.tag(t, id(inv)))
	if cleared["tax_date"] != nil {
		t.Errorf("cleared tax date: got %v, want none", cleared["tax_date"])
	}
	e.Problem(422, "payables.tax_mismatch", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-04-06"}`, e.tag(t, id(inv)))
	e.Must(200, "PATCH", "/v1/supplier-invoices/"+id(inv), `{"tax_date": "2026-03-20"}`, e.tag(t, id(inv)))
	entry := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-04-06"}`)["journal_entry_id"]
	if got := taxItems(t, entry); got != "R9/tax 10000 1000 1000 at 10 on 2026-03-20" {
		t.Errorf("items: got %s, want R9 at 10%% on 2026-03-20", got)
	}
}

// TestReverseCharge: a reverse charge code's supplier charges no tax; the
// invoice self-assesses it, owed on the output account and recovered on
// the input account, two items netting to zero.
func TestReverseCharge(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "RC-1", "100.00", "0.00", coded(e, "100.00", "RC")), "")
	entry := e.postedEntry(t, inv)
	output := e.Account(t, "2000", "240000").String()
	want := []line{{e.expense.String(), "100.00", nil}, {output, "-5.00", nil}, {e.tax.String(), "5.00", nil}, {e.ap.String(), "-100.00", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got, want := taxItems(t, entry), "RC/claim 10000 500 500 at 5 on 2026-03-10; RC/due 10000 -500 0 at 5 on 2026-03-10"; got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
}

// TestForeignInvoiceTaxAtTheTaxDate: a USD invoice's tax lines are
// converted at the rate of its tax date, the invoice date, its other
// lines at the posting date's, and what that leaves is an exchange
// difference (ADR-0049 §8); its items' local amounts are the tax date's.
func TestForeignInvoiceTaxAtTheTaxDate(t *testing.T) {
	e := setup(t)
	e.GRIR(t) // the fx_gain and fx_loss accounts
	for _, r := range []struct{ from, rate string }{{"2026-01-01", "0.9"}, {"2026-03-15", "0.8"}} {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', $2, $3, 'ECB', $4)`, e.Tenant.ID, r.from, r.rate, e.Tenant.Human.ID)
	}
	body := strings.ReplaceAll(e.invoice(e.Supplier, "USD-1", "119.00", "19.00", accountItem(e.expense, "100.00")), `"EUR"`, `"USD"`)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", body, "")
	entry := e.postedEntry(t, inv)
	var got string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_tc || '/' || l.amount_lc, ', ' ORDER BY a.number, l.amount_tc)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.id = l.account_id WHERE l.journal_entry_id = $1
			AND (l.amount_tc <> 0 OR l.amount_lc <> 0)`, []any{entry}, &got) // the group currency's own difference line aside
	if want := "140000 1900/1710, 211000 -11900/-9520, 480000 0/-190, 520000 10000/8000"; got != want {
		t.Errorf("lines (tc/lc): got %s, want %s", got, want)
	}
	var items string
	pgtest.Scan(t, `SELECT string_agg(base_lc || '/' || tax_lc, '; ') FROM ledger.tax_item WHERE journal_entry_id = $1`, []any{entry}, &items)
	if items != "9000/1710" {
		t.Errorf("items' local base and tax: got %s, want 9000/1710 at the tax date's 0.9", items)
	}
}

// TestCodeWithdrawnBeforePosting: a parked invoice whose code has no rate
// on its tax date any more (its rate replaced by a later one) is refused
// at posting with 422 payables.unknown_tax_code.
func TestCodeWithdrawnBeforePosting(t *testing.T) {
	e := setup(t)
	doc := inputCodes(t, e)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "LATE", "107.00", "7.00", coded(e, "100.00", "V7")), "")
	fixture.TaxCodes(t, e.Tenant, doc, config.TaxCode{Code: "V7", Description: "Reduced", Components: []config.TaxComponent{
		{Key: "tax", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{rate("7", "2026-04-01", "")}}}})
	// Read, the invoice leaves its figures out (T73e3): posting says why.
	if read, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", ""); read["calculated_tax"] != nil {
		t.Errorf("read: got calculated_tax %v, want none without a rate", read["calculated_tax"])
	}
	e.Problem(422, "payables.unknown_tax_code", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
}

// TestCodeNotCalculable: an invoice whose code the calculator cannot
// calculate on its tax date is refused with 422 payables.invalid_tax_code
// naming the code, not a 500: a reverse charge code whose self-assessed
// half has no rate on the tax date yet, so the code read then is half a
// pair. The invoice read leaves its figures out (T73e3).
func TestCodeNotCalculable(t *testing.T) {
	e := setup(t)
	fixture.TaxCodes(t, e.Tenant, inputCodes(t, e), halfPair("RX"))
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "HALF", "100.00", "0.00", coded(e, "100.00", "RX")), "")
	if read, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", ""); read["calculated_tax"] != nil {
		t.Errorf("read: got calculated_tax %v, want none", read["calculated_tax"])
	}
	p := e.Problem(422, "payables.invalid_tax_code", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
	if detail := fmt.Sprint(p["detail"]); !strings.Contains(detail, "RX") {
		t.Errorf("detail: got %s, want the code named", detail)
	}
}

// halfPair is a reverse charge code whose self-assessed output has a rate
// from April 2026 only: on an earlier date the code read is its claim
// alone, which the calculator cannot calculate.
func halfPair(code string) config.TaxCode {
	return config.TaxCode{Code: code, Description: "Half a pair", Kind: config.TaxReverseCharge, Components: []config.TaxComponent{
		{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: []config.TaxRate{rate("5", "2026-04-01", "")}},
		{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", Rates: []config.TaxRate{rate("5", "2026-01-01", "")}}}}
}

// TestTaxTooLarge: an invoice whose tax's local amounts at the tax date's
// rate are out of range is refused with 422 payables.amount_too_large.
func TestTaxTooLarge(t *testing.T) {
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 2, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	body := strings.ReplaceAll(e.invoice(e.Supplier, "HUGE", "59500000000000000.00", "9500000000000000.00",
		accountItem(e.expense, "50000000000000000.00")), `"EUR"`, `"USD"`)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", body, "")
	e.Problem(422, "payables.amount_too_large", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
}

// TestForeignInvoiceWithoutFXAccounts: the exchange difference a foreign
// invoice's tax lines leave needs the legal entity's fx accounts (422
// ledger.no_fx_account without them).
func TestForeignInvoiceWithoutFXAccounts(t *testing.T) {
	e := setup(t)
	for _, r := range []struct{ from, rate string }{{"2026-01-01", "0.9"}, {"2026-03-15", "0.8"}} {
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', $2, $3, 'ECB', $4)`, e.Tenant.ID, r.from, r.rate, e.Tenant.Human.ID)
	}
	body := strings.ReplaceAll(e.invoice(e.Supplier, "USD-2", "119.00", "19.00", accountItem(e.expense, "100.00")), `"EUR"`, `"USD"`)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", body, "")
	e.Problem(422, "ledger.no_fx_account", "POST", "/v1/supplier-invoices/"+id(inv)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(inv)))
}

// TestPostingTaxDatabaseFailures: every database read posting an invoice's
// tax makes, with codes and without, reports its failure.
func TestPostingTaxDatabaseFailures(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	for name, item := range map[string]string{"coded": coded(e, "20.00", "M5"), "uncoded": untaxedItem(e.expense, "20.00")} {
		t.Run(name, func(t *testing.T) {
			inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "FAIL-"+name, "23.80", "3.80", item), "")
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post "+name, func(ctx context.Context) error {
				_, err := endpoints(nil).PostInvoice(ctx, uuid.MustParse(id(inv)), func(int32) error { return nil }, nil)
				return err
			})
		})
	}
}

// TestInvoiceWithoutCodes: an invoice whose items carry no code posts its
// stated tax on the input tax account, without tax items.
func TestInvoiceWithoutCodes(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "NO-CODE", "105.00", "5.00", untaxedItem(e.expense, "100.00")), "")
	entry := e.postedEntry(t, inv)
	want := []line{{e.expense.String(), "100.00", nil}, {e.tax.String(), "5.00", nil}, {e.ap.String(), "-105.00", nil}}
	if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("lines: got %v, want %v", sorted(got), sorted(want))
	}
	if got := taxItems(t, entry); got != "" {
		t.Errorf("items: got %s, want none", got)
	}
}
