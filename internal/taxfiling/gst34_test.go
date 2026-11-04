package taxfiling_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/packs"
)

// The golden GST/HST return (T61j): Maple Inc (company 3000, CAD) adopts
// the released Canada pack 2026.6, whose returns.yaml defines the GST34's
// lines, and files them under its quarterly CRA registration. Its codes
// are the pack's purchase codes and the sales, self-assessed, import and
// adjustment codes a Canadian tenant adds, keyed as the pack maps them
// (packs/ca/TAX-PENDING-T61j.md).

// gst34Codes are company 3000's tax codes: the pack's own, on its chart
// (1300 and 1310 are 140000 and 141000 here), then the tenant's.
func gst34Codes(t *testing.T) []config.TaxCode {
	t.Helper()
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ca := lib.Get("CA", "2026.6")
	if ca == nil {
		t.Fatal("Get(CA, 2026.6): got nil, want the pack with the GST34")
	}
	accounts := map[string]string{"1300": "140000", "1310": "141000"}
	var codes []config.TaxCode
	for _, c := range ca.Tax.TaxCodes {
		c.Components = append([]config.TaxComponent(nil), c.Components...)
		for i := range c.Components {
			c.Components[i].Account = accounts[c.Components[i].Account]
		}
		codes = append(codes, c)
	}
	rate := func(pct string) []config.TaxRate {
		return []config.TaxRate{{RatePercent: pct, ValidFrom: "2026-01-01"}}
	}
	part := func(key, jurisdiction, accountKey, account, pct string) config.TaxComponent {
		return config.TaxComponent{Key: key, Jurisdiction: jurisdiction, AccountKey: accountKey, Account: account, Rates: rate(pct)}
	}
	code := func(c, direction, kind string, parts ...config.TaxComponent) config.TaxCode {
		return config.TaxCode{Code: c, Description: c, Direction: direction, Kind: kind, Components: parts}
	}
	claim := part("hst", "CA-ON", config.KeySelfInput, "140000", "13")
	claim.RecoverablePercent = "60"
	return append(codes,
		code("GS5", config.TaxOutput, config.TaxStandard, part("gst", "CA", config.KeyOutput, "240000", "5")),
		code("HS13", config.TaxOutput, config.TaxStandard, part("hst", "CA-ON", config.KeyOutput, "240000", "13")),
		code("GQ", config.TaxOutput, config.TaxStandard, part("gst", "CA", config.KeyOutput, "240000", "5"),
			part("qst", "CA-QC", config.KeyOutput, "241000", "9.975")),
		code("ZR", config.TaxOutput, config.TaxZero, part("gst", "CA", config.KeyOutput, "240000", "0")),
		code("EX", config.TaxOutput, config.TaxExempt, part("gst", "CA", config.KeyOutput, "240000", "0")),
		code("OS", config.TaxOutput, config.TaxOutOfScope, part("gst", "CA", config.KeyOutput, "240000", "0")),
		// Imported services used 60% in commercial activities: line 405,
		// the part recovered an ITC (ADR-0049 §9's worked case).
		code("RC4", config.TaxInput, config.TaxReverseCharge, part("hst_405", "CA-ON", config.KeySelfOutput, "240000", "13"), claim),
		code("RP5", config.TaxInput, config.TaxReverseCharge, part("gst_205", "CA", config.KeySelfOutput, "240000", "5"),
			part("gst", "CA", config.KeySelfInput, "140000", "5")),
		code("IC5", config.TaxInput, config.TaxImport, part("gst", "CA", config.KeyInput, "140000", "5")),
		code("AI", config.TaxOutput, config.TaxAdjustment, part("gst", "CA", config.KeyOutput, "240000", "0")),
		code("AD", config.TaxInput, config.TaxAdjustment, part("gst", "CA", config.KeyInput, "140000", "0")),
		code("IN", config.TaxInput, config.TaxAdjustment, part("instalment", "CA", config.KeyInput, "140200", "0")),
		code("RB", config.TaxInput, config.TaxAdjustment, part("rebate", "CA", config.KeyInput, "140300", "0")))
}

// gst34Env is a tenant with Maple Inc on the Canada pack 2026.6, its
// GST/HST registration GSTHST quarterly from 2026-01-01, and CAD's rate
// into the group's EUR.
func gst34Env(t *testing.T) (fixture.Env, uuid.UUID) {
	t.Helper()
	e := fixture.NewWith(t, []fixture.Company{fixture.MapleCompany}, func(doc *config.Config) {
		fixture.Maple(doc)
		co := doc.Company("3000")
		co.ChartOfAccounts = append(co.ChartOfAccounts,
			config.Account{Number: "240000", Name: "GST/HST payable", Type: config.Liability},
			config.Account{Number: "241000", Name: "QST payable", Type: config.Liability},
			config.Account{Number: "141000", Name: "QST receivable", Type: config.Asset},
			config.Account{Number: "140200", Name: "GST/HST instalments paid", Type: config.Asset},
			config.Account{Number: "140300", Name: "GST/HST rebates receivable", Type: config.Asset},
			config.Account{Number: "219500", Name: "Customs clearing", Type: config.Liability})
		co.TaxCodes = gst34Codes(t)
		co.TaxRegistrations = []config.TaxRegistration{{Code: "GSTHST", Authority: "CRA", Number: "123456789 RT 0001", ReturnType: "GST34",
			Frequency: config.TaxQuarterly, FirstPeriodStart: "2026-01-01"}}
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: "3000", Country: "CA", Version: "2026.6", AppliedOn: "2026-01-01"})
	}, gst34Perms()...)
	var le uuid.UUID
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = '3000'`, []any{e.Tenant.ID}, &le)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'CAD', 'EUR', '2026-01-01', 0.68, 'BOC', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e, le
}

// gst34Perms are the return's, the lifecycle's and the invoices'
// permissions, each once.
func gst34Perms() []string {
	var out []string
	for _, p := range slices.Concat(returnPerms, lifecyclePerms, fixture.InvoicePermissions) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// entry posts a journal entry of company 3000 in CAD on the day, its lines
// "account amount [code [declared base]]", and returns its id and ETag.
func (g gst34) entry(t *testing.T, day string, lines ...string) (id, etag string) {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		f := strings.Fields(l)
		extra := ""
		if len(f) > 2 {
			extra = fmt.Sprintf(`, "tax_code": %q`, f[2])
		}
		if len(f) > 3 {
			extra += fmt.Sprintf(`, "tax_base": {"amount": %q, "currency": "CAD"}`, f[3])
		}
		out[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "CAD"}%s}`, g.e.Account(t, "3000", f[0]), f[1], extra)
	}
	je, tag := g.e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q,
		"currency": "CAD", "lines": [%s]}`, g.le, day, day, strings.Join(out, ", ")), "")
	return je["id"].(string), tag
}

// invoice posts a supplier invoice of company 3000 in CAD dated the day,
// its items "amount code" on expense account 520000, with the stated tax.
func (g gst34) invoice(t *testing.T, ref, day, gross, tax string, items ...string) {
	t.Helper()
	g.invoiceOn(t, ref, day, day, gross, tax, items...)
}

// invoiceOn is invoice posted on another day than its date.
func (g gst34) invoiceOn(t *testing.T, ref, day, posted, gross, tax string, items ...string) {
	t.Helper()
	out := make([]string, len(items))
	for i, it := range items {
		f := strings.Fields(it)
		out[i] = fmt.Sprintf(`{"account_id": %q, "description": "Item %d", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "CAD"}, "tax_code": %q}`,
			g.e.Account(t, "3000", "520000"), i+1, f[0], f[1])
	}
	inv, tag := g.e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "gross_amount": {"amount": %q, "currency": "CAD"}, "tax_amount": {"amount": %q, "currency": "CAD"}, "items": [%s]}`,
		g.le, g.e.Supplier, ref, day, gross, tax, strings.Join(out, ", ")), "")
	g.e.Must(200, "POST", "/v1/supplier-invoices/"+inv["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, posted), tag)
}

type gst34 struct {
	e  fixture.Env
	le uuid.UUID
}

// worked posts the worked set of the first quarter of 2026: sales of each
// kind and a credit, purchases by supplier invoice (GST, HST, QST and BC
// PST; self-assessed imported services and real property), the GST the
// CBSA collected on an import, the adjustments, an instalment and a
// rebate, and a sale posted twice and reversed.
func (g gst34) worked(t *testing.T) {
	t.Helper()
	g.entry(t, "2026-01-15", "100000 1130.00", "400000 -1000.00 HS13", "240000 -130.00 HS13")              // an Ontario sale, HST 13%
	g.entry(t, "2026-01-20", "100000 2100.00", "400000 -2000.00 GS5", "240000 -100.00 GS5")                // a sale with GST 5%
	g.entry(t, "2026-01-25", "100000 500.00", "400000 -500.00 ZR")                                         // a zero-rated export
	g.entry(t, "2026-01-26", "100000 300.00", "400000 -300.00 EX")                                         // an exempt supply
	g.entry(t, "2026-01-27", "100000 400.00", "400000 -400.00 OS")                                         // a supply out of scope: on no line
	g.entry(t, "2026-02-03", "100000 919.80", "400000 -800.00 GQ", "240000 -40.00 GQ", "241000 -79.80 GQ") // a Quebec sale: its QST on no line
	g.entry(t, "2026-02-10", "400000 200.00 GS5", "240000 10.00 GS5", "100000 -210.00")                    // a credit note on the GST sale
	twice, tag := g.entry(t, "2026-02-12", "100000 105.00", "400000 -100.00 GS5", "240000 -5.00 GS5")      // posted twice by mistake
	g.e.Must(200, "POST", "/v1/journal-entries/"+twice+":reverse", "", tag)
	g.invoice(t, "P1", "2026-02-15", "1615.00", "115.00", "1000.00 G5", "500.00 HON") // ITCs: 50.00 and 65.00
	g.invoice(t, "P2", "2026-02-16", "216.98", "16.98", "100.00 QST", "100.00 PBC")   // QST refund and BC PST expensed: on no line
	g.invoice(t, "P3", "2026-02-20", "1000.00", "0.00", "1000.00 RC4")                // imported services: 130.00 owed, 78.00 claimed
	g.invoice(t, "P4", "2026-02-21", "2000.00", "0.00", "2000.00 RP5")                // real property: 100.00 owed and claimed
	g.entry(t, "2026-03-02", "140000 650.00 IC5 13000.00", "219500 -650.00")          // the CBSA's GST on an import
	g.entry(t, "2026-03-10", "520000 25.00", "240000 -25.00 AI")                      // an adjustment increasing the net tax
	g.entry(t, "2026-03-11", "140000 40.00 AD", "520000 -40.00")                      // an adjustment decreasing it
	g.entry(t, "2026-03-12", "140200 300.00 IN", "100000 -300.00")                    // an instalment paid
	g.entry(t, "2026-03-13", "140300 15.00 RB", "400000 -15.00")                      // a rebate claimed
}

// gst34Lines are the lines of the worked set's return, in the pack's
// order, as code=amount/prior-period part[#items].
const gst34Lines = "101=4400.00/0.00#8 103=260.00/0.00#6 104=25.00/0.00#1 105=285.00/0.00 106=943.00/0.00#5 107=40.00/0.00#1 " +
	"108=983.00/0.00 109=-698.00/0.00 110=300.00/0.00#1 111=15.00/0.00#1 112=315.00/0.00 113a=-1013.00/0.00 " +
	"205=100.00/0.00#1 405=130.00/0.00#1 113b=230.00/0.00 113c=-783.00/0.00 114=783.00/0.00 115=0.00/0.00"

// TestGST34Golden: the worked set's GST/HST return through the return
// engine with the released pack: each line sums what the pack maps to it
// (QST, BC PST and the out-of-scope supply on none), the reversal cancels the
// sale it reverses, and the subtotals give a refund. Prepared, the return
// keeps the lines in the pack's order and claims the items some line
// selects, leaving the QST and PST items to another registration. The
// next quarter owes, with a late ITC shown apart.
func TestGST34Golden(t *testing.T) {
	e, le := gst34Env(t)
	g := gst34{e: e, le: le}
	g.worked(t)
	reg := id(t, e, "GSTHST")
	draft, _ := e.Must(200, "GET", compute(reg, "2026-01-01"), "", "")
	want(t, "the draft's lines", boxes(draft), gst34Lines)
	want(t, "the draft", fmt.Sprintf("%v %v %v %v", draft["return_type"], draft["return_name"], draft["currency"], draft["pack"]),
		"GST34 GST/HST return (GST34 lines) CAD map[country:CA version:2026.6]")
	// 405 drills down to the owed part of the imported services.
	items, _ := e.Must(200, "GET", "/v1/tax-registrations/"+reg+"/draft-return/items?period_start=2026-01-01&box=405", "", "")
	it := items["items"].([]any)[0].(map[string]any)
	want(t, "line 405's item", fmt.Sprintf("%v/%v %v %v", it["tax_code"], it["component"], it["account_key"], it["box_amount"].(map[string]any)["amount"]),
		"RC4/hst_405 self_output 130.00")

	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period BETWEEN 1 AND 3`, e.Tenant.ID, le)
	ret, _ := e.Must(201, "POST", returnsOf(reg), `{"period_start": "2026-01-01"}`, "")
	var lines []string
	for _, b := range strings.Fields(snapshot(ret)) {
		code, rest, _ := strings.Cut(b, "=")
		amount, _, _ := strings.Cut(rest, "/")
		lines = append(lines, code+"="+amount)
	}
	want(t, "the prepared return's lines", strings.Join(lines, " "),
		"101=4400.00 103=260.00 104=25.00 105=285.00 106=943.00 107=40.00 108=983.00 109=-698.00 110=300.00 111=15.00 112=315.00 "+
			"113a=-1013.00 205=100.00 405=130.00 113b=230.00 113c=-783.00 114=783.00 115=0.00")
	var unclaimed string
	pgtest.Scan(t, `SELECT string_agg(code || '/' || component, ' ' ORDER BY code, component) FROM ledger.tax_item
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND reported_in IS NULL`, []any{e.Tenant.ID, le}, &unclaimed)
	want(t, "the items no line selects", unclaimed, "GQ/qst OS/gst PBC/pst_bc QST/qst")

	// The second quarter owes: an Ontario sale, and a supplier's invoice of
	// 30 March that arrived in April, its ITC an earlier period's (P7),
	// whose own return is prepared.
	g.entry(t, "2026-04-15", "100000 1130.00", "400000 -1000.00 HS13", "240000 -130.00 HS13")
	g.invoiceOn(t, "P5", "2026-03-30", "2026-04-10", "210.00", "10.00", "200.00 G5")
	q2, _ := e.Must(200, "GET", compute(reg, "2026-04-01"), "", "")
	want(t, "the second quarter's lines", boxes(q2), "101=1000.00/0.00#1 103=130.00/0.00#1 104=0.00/0.00#0 105=130.00/0.00 106=10.00/10.00#1 "+
		"107=0.00/0.00#0 108=10.00/10.00 109=120.00/-10.00 110=0.00/0.00#0 111=0.00/0.00#0 112=0.00/0.00 113a=120.00/-10.00 205=0.00/0.00#0 "+
		"405=0.00/0.00#0 113b=0.00/0.00 113c=120.00/-10.00 114=0.00/10.00 115=120.00/0.00")
}
