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

// The golden VAT 201 (T61k): Maple Gulf Trading (company 5000, AED)
// adopts the released UAE pack 2026.2, whose returns.yaml defines the VAT
// 201's boxes, and files them under its quarterly FTA registration. Its
// codes are the pack's and the adjustment codes a UAE tenant adds, keyed
// as the pack maps them (packs/ae/TAX-PENDING-T61k.md).

// vat201Company is Maple Gulf Trading's legal entity.
var vat201Company = fixture.Company{Code: "5000", Name: "Maple Gulf Trading", Country: "AE", Currency: "AED"}

// vat201Codes are company 5000's tax codes: the pack's own, on its chart
// (1300 and 2300 are 140000 and 240000 here), then the tenant's
// adjustment codes: AS by emirate (box 1's adjustment column), AP (box
// 9's), TR (box 2) and IA (box 7).
func vat201Codes(t *testing.T) []config.TaxCode {
	t.Helper()
	lib, err := countrypack.LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	ae := lib.Get("AE", "2026.2")
	if ae == nil {
		t.Fatal("Get(AE, 2026.2): got nil, want the pack with the VAT 201")
	}
	accounts := map[string]string{"1300": "140000", "2300": "240000"}
	var codes []config.TaxCode
	for _, c := range ae.Tax.TaxCodes {
		c.Components = append([]config.TaxComponent(nil), c.Components...)
		for i := range c.Components {
			c.Components[i].Account = accounts[c.Components[i].Account]
		}
		codes = append(codes, c)
	}
	adjustment := func(code, direction, key, accountKey, account string, onLine bool) config.TaxCode {
		return config.TaxCode{Code: code, Description: code, Direction: direction, Kind: config.TaxAdjustment,
			Components: []config.TaxComponent{{Key: key, Jurisdiction: "AE", JurisdictionOnLine: onLine, AccountKey: accountKey, Account: account,
				Rates: []config.TaxRate{{RatePercent: "0", ValidFrom: "2026-01-01"}}}}}
	}
	return append(codes,
		adjustment("AS", config.TaxOutput, "vat", config.KeyOutput, "240000", true),
		adjustment("AP", config.TaxInput, "vat", config.KeyInput, "140000", false),
		adjustment("TR", config.TaxOutput, "tourist_refund", config.KeyOutput, "240000", false),
		adjustment("IA", config.TaxOutput, "import_adjustment", config.KeyOutput, "240000", false))
}

// vat201Env is a tenant with Maple Gulf Trading on the UAE pack 2026.2 (a
// copy of company 2000's settings in AED, its limits in dirhams), its FTA registration VAT
// quarterly from 2026-01-01, and AED's rate into the group's EUR.
func vat201Env(t *testing.T) (fixture.Env, uuid.UUID) {
	t.Helper()
	e := fixture.NewWith(t, []fixture.Company{vat201Company}, func(doc *config.Config) {
		co := *doc.Company("2000")
		co.Code, co.Name, co.Currency, co.Country = vat201Company.Code, vat201Company.Name, vat201Company.Currency, vat201Company.Country
		co.ChartOfAccounts = append(slices.Clone(co.ChartOfAccounts),
			config.Account{Number: "240000", Name: "VAT payable", Type: config.Liability},
			config.Account{Number: "219500", Name: "Customs clearing", Type: config.Liability})
		co.TaxCodes = vat201Codes(t)
		co.ManualEntryApprovalLimit, co.AutoPostLimit = "50000.00", "500000.00"
		co.TaxRegistrations = []config.TaxRegistration{{Code: "VAT", Authority: "FTA", Number: "100000000000003", ReturnType: "VAT201",
			Frequency: config.TaxQuarterly, FirstPeriodStart: "2026-01-01"}}
		doc.Companies = append(doc.Companies, co)
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: "5000", Country: "AE", Version: "2026.2", AppliedOn: "2026-01-01"})
	}, gst34Perms()...)
	var le uuid.UUID
	pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = '5000'`, []any{e.Tenant.ID}, &le)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'AED', 'EUR', '2026-01-01', 0.25, 'CBUAE', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e, le
}

type vat201 struct {
	e  fixture.Env
	le uuid.UUID
}

// entry posts a journal entry of company 5000 in AED on the day, its lines
// "account amount [code[@emirate] [declared base]]", and returns its id.
func (g vat201) entry(t *testing.T, day string, lines ...string) string {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		f := strings.Fields(l)
		extra := ""
		if len(f) > 2 {
			code, emirate, onLine := strings.Cut(f[2], "@")
			extra = fmt.Sprintf(`, "tax_code": %q`, code)
			if onLine {
				extra += fmt.Sprintf(`, "tax_jurisdiction": %q`, emirate)
			}
		}
		if len(f) > 3 {
			extra += fmt.Sprintf(`, "tax_base": {"amount": %q, "currency": "AED"}`, f[3])
		}
		out[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "AED"}%s}`, g.e.Account(t, "5000", f[0]), f[1], extra)
	}
	je, _ := g.e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q,
		"currency": "AED", "lines": [%s]}`, g.le, day, day, strings.Join(out, ", ")), "")
	return je["id"].(string)
}

// invoice posts a supplier invoice of company 5000 in AED dated the day
// and posted on another, one item "amount code" on expense account 520000,
// with the stated tax.
func (g vat201) invoice(t *testing.T, ref, day, posted, gross, tax, item string) string {
	t.Helper()
	return g.document(t, "", ref, day, posted, gross, tax, item)
}

// creditMemo is invoice's credit memo of the invoice credited.
func (g vat201) creditMemo(t *testing.T, credited, ref, day, posted, gross, tax, item string) {
	t.Helper()
	g.document(t, fmt.Sprintf(`"kind": "credit_memo", "reference_invoice_id": %q, `, credited), ref, day, posted, gross, tax, item)
}

// document posts invoice's supplier document, its kind's fields extra,
// and returns its id.
func (g vat201) document(t *testing.T, extra, ref, day, posted, gross, tax, item string) string {
	t.Helper()
	f := strings.Fields(item)
	inv, tag := g.e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{%s"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": %q, "gross_amount": {"amount": %q, "currency": "AED"}, "tax_amount": {"amount": %q, "currency": "AED"},
		"items": [{"account_id": %q, "description": "Item", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": %q, "currency": "AED"}, "tax_code": %q}]}`,
		extra, g.le, g.e.Supplier, ref, day, gross, tax, g.e.Account(t, "5000", "520000"), f[0], f[1]), "")
	g.e.Must(200, "POST", "/v1/supplier-invoices/"+inv["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, posted), tag)
	return inv["id"].(string)
}

// worked posts the worked set of the first quarter of 2026: standard-rated
// sales in three emirates and a credit note, a sale in Ajman posted twice
// and reversed, zero-rated, exempt and out-of-scope supplies; purchases
// with recoverable and blocked VAT, imported services and the domestic
// reverse charge; import VAT from a customs declaration; and an
// adjustment for each of boxes 1 (Dubai), 2, 7 and 9. It returns the
// import's entry.
func (g vat201) worked(t *testing.T) (imported string) {
	t.Helper()
	g.entry(t, "2026-01-15", "100000 10500.00", "400000 -10000.00 S5@AE-DU", "240000 -500.00 S5@AE-DU") // 1b
	g.entry(t, "2026-01-20", "100000 4200.00", "400000 -4000.00 S5@AE-AZ", "240000 -200.00 S5@AE-AZ")   // 1a
	g.entry(t, "2026-01-22", "100000 2100.00", "400000 -2000.00 S5@AE-SH", "240000 -100.00 S5@AE-SH")   // 1c
	g.entry(t, "2026-02-02", "400000 1000.00 S5@AE-DU", "240000 50.00 S5@AE-DU", "100000 -1050.00")     // a credit note in Dubai
	twice := g.entry(t, "2026-02-03", "100000 1050.00", "400000 -1000.00 S5@AE-AJ", "240000 -50.00 S5@AE-AJ")
	_, tag := g.e.Must(200, "GET", "/v1/journal-entries/"+twice, "", "")
	g.e.Must(200, "POST", "/v1/journal-entries/"+twice+":reverse", "", tag)                          // posted twice by mistake: 1d nets to nothing
	g.entry(t, "2026-01-25", "100000 3000.00", "400000 -3000.00 Z0")                                 // a zero-rated export: box 4
	g.entry(t, "2026-01-26", "100000 1500.00", "400000 -1500.00 E0")                                 // an exempt supply: box 5
	g.entry(t, "2026-01-27", "100000 800.00", "400000 -800.00 OS")                                   // out of scope: on no box
	g.invoice(t, "P1", "2026-02-15", "2026-02-15", "6300.00", "300.00", "6000.00 V5")                // box 9: 300.00 recovered
	g.invoice(t, "P2", "2026-02-16", "2026-02-16", "1050.00", "50.00", "1000.00 VB5")                // blocked: its amount in box 9, no VAT
	g.invoice(t, "P3", "2026-02-20", "2026-02-20", "4000.00", "0.00", "4000.00 RC5")                 // imported services: box 3 and 10
	g.invoice(t, "P4", "2026-02-21", "2026-02-21", "2000.00", "0.00", "2000.00 DR5")                 // domestic reverse charge: box 3 and 10
	imported = g.entry(t, "2026-03-02", "140000 500.00 IM5 10000.00", "240000 -500.00 IM5 10000.00") // customs: box 6 and 10
	g.entry(t, "2026-03-10", "520000 30.00", "240000 -30.00 AS@AE-DU")                               // box 1b's adjustment
	g.entry(t, "2026-03-11", "240000 20.00 TR", "100000 -20.00")                                     // a tourist refund: box 2
	g.entry(t, "2026-03-12", "520000 40.00", "240000 -40.00 IA")                                     // box 7
	g.entry(t, "2026-03-13", "140000 25.00 AP", "520000 -25.00")                                     // box 9's adjustment
	return imported
}

// vat201Boxes are the worked set's boxes, in the pack's order, as
// code=amount/prior-period part[#items]: 1,600.00 due, 1,125.00
// recoverable, 475.00 payable.
const vat201Boxes = "1a_amount=4000.00/0.00#1 1a_vat=200.00/0.00#1 1a_adjustment=0.00/0.00#0 " +
	"1b_amount=9000.00/0.00#2 1b_vat=450.00/0.00#2 1b_adjustment=30.00/0.00#1 " +
	"1c_amount=2000.00/0.00#1 1c_vat=100.00/0.00#1 1c_adjustment=0.00/0.00#0 " +
	"1d_amount=0.00/0.00#2 1d_vat=0.00/0.00#2 1d_adjustment=0.00/0.00#0 " +
	"1e_amount=0.00/0.00#0 1e_vat=0.00/0.00#0 1e_adjustment=0.00/0.00#0 " +
	"1f_amount=0.00/0.00#0 1f_vat=0.00/0.00#0 1f_adjustment=0.00/0.00#0 " +
	"1g_amount=0.00/0.00#0 1g_vat=0.00/0.00#0 1g_adjustment=0.00/0.00#0 " +
	"2_vat=-20.00/0.00#1 3_amount=6000.00/0.00#2 3_vat=300.00/0.00#2 4_amount=3000.00/0.00#1 5_amount=1500.00/0.00#1 " +
	"6_amount=10000.00/0.00#1 6_vat=500.00/0.00#1 7_vat=40.00/0.00#1 8_amount=35500.00/0.00 8_vat=1570.00/0.00 8_adjustment=30.00/0.00 " +
	"9_amount=7000.00/0.00#2 9_vat=300.00/0.00#2 9_adjustment=25.00/0.00#1 10_vat=800.00/0.00#3 " +
	"11_amount=7000.00/0.00 11_vat=1100.00/0.00 11_adjustment=25.00/0.00 12=1600.00/0.00 13=1125.00/0.00 14=475.00/0.00"

// TestVAT201Golden: the worked set's VAT 201 through the return engine
// with the released pack: standard-rated supplies by the emirate each line
// names, the reversal cancelling the sale it reverses, zero-rated and
// exempt supplies, the reverse charge and import VAT owed and recovered,
// blocked VAT recovering nothing, and the adjustments in their boxes; the
// out-of-scope supply on none. Prepared, the return keeps the boxes in the
// pack's order and leaves the out-of-scope item unclaimed. Filed, it is
// amended; the next quarter shows late VAT apart, its net shortfall of
// payable tax held to the pack's AED 10,000 threshold.
func TestVAT201Golden(t *testing.T) {
	e, le := vat201Env(t)
	g := vat201{e: e, le: le}
	imported := g.worked(t)
	reg := id(t, e, "VAT")
	draft, _ := e.Must(200, "GET", compute(reg, "2026-01-01"), "", "")
	want(t, "the draft's boxes", boxes(draft), vat201Boxes)
	want(t, "the draft", fmt.Sprintf("%v %v %v %v", draft["return_type"], draft["return_name"], draft["currency"], draft["pack"]),
		"VAT201 VAT return (VAT 201 boxes) AED map[country:AE version:2026.2]")
	// Box 1b drills down to the Dubai sale and its credit note.
	items, _ := e.Must(200, "GET", "/v1/tax-registrations/"+reg+"/draft-return/items?period_start=2026-01-01&box=1b_vat", "", "")
	var dubai []string
	for _, it := range items["items"].([]any) {
		m := it.(map[string]any)
		dubai = append(dubai, fmt.Sprintf("%v %v", m["tax_code"], m["box_amount"].(map[string]any)["amount"]))
	}
	want(t, "box 1b's items", strings.Join(dubai, ", "), "S5 500.00, S5 -50.00")

	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period BETWEEN 1 AND 3`, e.Tenant.ID, le)
	ret, _ := e.Must(201, "POST", returnsOf(reg), `{"period_start": "2026-01-01"}`, "")
	var prepared, drafted []string
	for _, b := range strings.Fields(snapshot(ret)) {
		code, rest, _ := strings.Cut(b, "=")
		amount, _, _ := strings.Cut(rest, "/")
		prepared = append(prepared, code+"="+amount)
	}
	for _, b := range strings.Fields(vat201Boxes) {
		code, rest, _ := strings.Cut(b, "=")
		amount, _, _ := strings.Cut(rest, "/")
		drafted = append(drafted, code+"="+amount)
	}
	want(t, "the prepared return's boxes", strings.Join(prepared, " "), strings.Join(drafted, " "))
	var unclaimed string
	pgtest.Scan(t, `SELECT string_agg(code || '/' || component, ' ' ORDER BY code, component) FROM ledger.tax_item
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND reported_in IS NULL`, []any{e.Tenant.ID, le}, &unclaimed)
	want(t, "the items no box selects", unclaimed, "OS/vat")
	vat201Amended(t, g, ret["id"].(string), reg, imported)

	// The second quarter: a sale in Dubai, and a supplier's invoice of 31
	// March that arrived in May, its VAT an earlier period's (P7), under
	// the pack's threshold; then one whose VAT exceeds it.
	g.entry(t, "2026-04-15", "100000 5250.00", "400000 -5000.00 S5@AE-DU", "240000 -250.00 S5@AE-DU")
	g.invoice(t, "P6", "2026-03-31", "2026-05-05", "1050.00", "50.00", "1000.00 V5")
	q2, _ := e.Must(200, "GET", compute(reg, "2026-04-01"), "", "")
	for code, figures := range map[string]string{"1b_vat": "250.00/0.00#1", "9_amount": "1000.00/1000.00#1", "9_vat": "50.00/50.00#1",
		"12": "250.00/0.00", "13": "50.00/50.00", "14": "200.00/-50.00"} {
		t.Run("second quarter's box "+code, func(t *testing.T) {
			if got := boxOf(q2, code); got != figures {
				t.Errorf("amount/prior part[#items]: got %s, want %s", got, figures)
			}
		})
	}
	prior := q2["prior_period"].(map[string]any)
	want(t, "the threshold, not exceeded", fmt.Sprintf("%v %v", prior["threshold"].(map[string]any)["amount"], prior["above_threshold"]), "10000.00 false")
	// The threshold is on a net shortfall of payable tax in one earlier
	// period, all its items together (po:2026-11-09, C11): 11,050.00 of
	// late input VAT is an overpayment and does not raise it; credit memos
	// of 31 March, posted in May, taking back 21,050.00 of input VAT make a
	// shortfall of exactly 10,000.00, which does not either; 0.01 more does.
	g.invoice(t, "P7", "2026-03-31", "2026-05-06", "210000.00", "10000.00", "200000.00 V5")
	p8 := g.invoice(t, "P8", "2026-03-31", "2026-05-07", "21000.00", "1000.00", "20000.00 V5")
	for _, c := range []struct{ name, gross, vat, base, want string }{
		{"an overpayment of 11,050.00", "", "", "", "false"},
		{"a shortfall of exactly 10,000.00", "442050.00", "21050.00", "421000.00", "false"},
		{"a shortfall of 10,000.01", "0.21", "0.01", "0.20", "true"},
	} {
		if c.vat != "" { // each case adds to the earlier ones' items
			g.creditMemo(t, p8, "C-"+c.vat, "2026-03-31", "2026-05-08", c.gross, c.vat, c.base+" V5")
		}
		// Not t.Run: the client fails the test it was made with.
		q2, _ = e.Must(200, "GET", compute(reg, "2026-04-01"), "", "")
		want(t, "above the threshold, "+c.name, fmt.Sprint(q2["prior_period"].(map[string]any)["above_threshold"]), c.want)
	}
}

// boxOf is one box of a draft as code=amount/prior-period part[#items].
func boxOf(m map[string]any, code string) string {
	for _, b := range strings.Fields(boxes(m)) {
		if c, rest, _ := strings.Cut(b, "="); c == code {
			return rest
		}
	}
	return ""
}

// vat201Amended files the first quarter's return and amends it with the
// reversal of the customs declaration's entry, posted in April with the
// original's tax date, and a supplier's invoice of 30 March posted in
// April: box 6 and its claim in box 10 fall away, box 9 rises, and the
// payable 475.00 becomes 375.00 (14's correction -100.00).
func vat201Amended(t *testing.T, g vat201, ret, reg, imported string) {
	t.Helper()
	e := g.e
	e.Approve(t, ret)
	e.Must(200, "POST", "/v1/tax-returns/"+ret+":file", `{"confirmation_number": "FTA-Q1", "filed_on": "2026-04-20"}`, `"2"`)
	_, tag := e.Must(200, "GET", "/v1/journal-entries/"+imported, "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+imported+":reverse", `{"posting_date": "2026-04-20"}`, tag)
	g.invoice(t, "P5", "2026-03-30", "2026-04-10", "2100.00", "100.00", "2000.00 V5")
	amendment, _ := e.Must(201, "POST", returnsOf(reg), `{"period_start": "2026-01-01", "kind": "amendment"}`, "")
	got := map[string]string{}
	for _, b := range strings.Fields(snapshot(amendment)) {
		code, rest, _ := strings.Cut(b, "=")
		got[code] = rest
	}
	for box, figures := range map[string]string{"6_amount": "-10000.00/0.00/0.00#1", "6_vat": "-500.00/0.00/0.00#1", "8_vat": "-500.00/0.00/1070.00",
		"9_amount": "2000.00/0.00/9000.00#1", "9_vat": "100.00/0.00/400.00#1", "10_vat": "-500.00/0.00/300.00#1",
		"12": "-500.00/0.00/1100.00", "13": "-400.00/0.00/725.00", "14": "-100.00/0.00/375.00"} {
		t.Run("amendment's box "+box, func(t *testing.T) {
			if got[box] != figures {
				t.Errorf("amount/prior part/corrected: got %s, want %s", got[box], figures)
			}
		})
	}
}
