package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// intercompanyGroup is fixture.Consolidation's group ACME (company 2000,
// in EUR, holding company 1000) with an intercompany payables account in
// company 2000, a vendor reconciliation account, and an intercompany
// receivables account in company 1000, each on its group account of the
// elimination IC_BALANCES.
func intercompanyGroup(doc *config.Config) {
	fixture.Consolidation(doc)
	de, us := doc.Company("2000"), doc.Company("1000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "225000", Name: "Intercompany payables", Type: config.Liability,
		VendorReconciliation: true, GroupAccount: "G2250"})
	us.ChartOfAccounts = append(us.ChartOfAccounts, config.Account{Number: "125000", Name: "Intercompany receivables", Type: config.Asset,
		GroupAccount: "G1250"})
}

// intercompanySetup is setup with intercompanyGroup's configuration, both
// companies on one fiscal calendar as a group's members are, USD at 0.90
// EUR, October's and November 2026's closing rates too, and people who may also run a
// consolidation.
func intercompanySetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, intercompanyGroup, append([]string{"consolidation.read", "consolidation.run.manage"}, perms...)...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.9, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	pgtest.Exec(t, `UPDATE tenancy.legal_entity u SET fiscal_calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.id = $1 AND d.id = $2`, f.US, f.DE)
	pgtest.Exec(t, `UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, f.US, f.DE)
	return env{Env: f, dir: dir}
}

// intercompanyVendor is the vendor of intercompany that is a company of
// the group.
const intercompanyVendor = "0000000009"

// intercompany is a scope of company 2000 with a vendor that is the
// group's company 1000: its master names company ID 001000 as its trading
// partner (LFA1-VBUND), which T001 assigns to company code 1000 (RCOMP),
// a company outside the scope. Its invoice of 90.00 on the intercompany
// payables account is open, its vendor line alone carrying the trading
// partner in SAP (ACDOCA-RASSC), the expense and tax lines none, as
// invoice verification leaves them; another of 40.00 is paid and cleared.
// An ordinary vendor's invoice of 30.00 is open on the domestic account.
func intercompany() *sap.Extract {
	line := func(belnr, docln, blart, account, hsl string, change func(l *sap.ACDOCA)) sap.ACDOCA {
		l := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, WSL: hsl,
			BUDAT: "20261105", BLART: blart}
		if change != nil {
			change(&l)
		}
		return l
	}
	group := func(augbl string) func(l *sap.ACDOCA) {
		return func(l *sap.ACDOCA) { l.LIFNR, l.RASSC, l.AUGBL = intercompanyVendor, "001000", augbl }
	}
	return &sap.Extract{
		T001: []sap.T001{{BUKRS: "2000", BUTXT: "Acme GmbH", WAERS: "EUR", LAND1: "DE", RCOMP: "002000"},
			{BUKRS: "1000", BUTXT: "Acme Corp US", WAERS: "USD", LAND1: "US", RCOMP: "001000"}},
		SKB1: []sap.SKB1{{BUKRS: "2000", SAKNR: "0000100000", TXT50: "Bank", XBILK: "X"}, {BUKRS: "2000", SAKNR: "0000140000", TXT50: "Input tax", XBILK: "X"},
			{BUKRS: "2000", SAKNR: "0000211000", TXT50: "AP", XBILK: "X", MITKZ: "K"},
			{BUKRS: "2000", SAKNR: "0000225000", TXT50: "Intercompany payables", XBILK: "X", MITKZ: "K"}, {BUKRS: "2000", SAKNR: "0000520000", TXT50: "Office"}},
		LFA1: []sap.LFA1{{LIFNR: "0000000001", NAME1: "Tiny GmbH", LAND1: "DE", STCD1: "DE123"},
			{LIFNR: intercompanyVendor, NAME1: "Acme Corp US", LAND1: "US", STCD1: "US-1", VBUND: "001000"}},
		LFB1: []sap.LFB1{{LIFNR: "0000000001", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"},
			{LIFNR: intercompanyVendor, BUKRS: "2000", AKONT: "0000225000", ZTERM: "NT30"}},
		ACDOCA: []sap.ACDOCA{
			line("0100000001", "000001", "RE", "0000520000", "75.00", nil),
			line("0100000001", "000002", "RE", "0000140000", "15.00", nil),
			line("0100000001", "000003", "RE", "0000225000", "-90.00", group("")),
			line("0100000002", "000001", "RE", "0000520000", "40.00", nil),
			line("0100000002", "000002", "RE", "0000225000", "-40.00", group("0100000003")),
			line("0100000003", "000001", "KZ", "0000225000", "40.00", group("0100000003")),
			line("0100000003", "000002", "KZ", "0000100000", "-40.00", nil),
			line("0100000004", "000001", "RE", "0000520000", "30.00", nil),
			line("0100000004", "000002", "RE", "0000211000", "-30.00", func(l *sap.ACDOCA) { l.LIFNR = "0000000001" }),
		},
	}
}

// partnerChecks are a report's checks by trading partner (the open
// payables and the account balances), with both sides' figures.
func partnerChecks(report map[string]any) string {
	var out []string
	for _, c := range report["checks"].([]any) {
		if m := c.(map[string]any); strings.Contains(m["name"].(string), "trading partner") {
			out = append(out, fmt.Sprintf("%s %s: %s / %s %v", m["company_code"], m["name"], m["sap"], m["platform"], m["pass"]))
		}
	}
	return strings.Join(out, "; ")
}

// tradingPartnerLines are company 2000's journal lines that name company
// 1000 as their trading partner, as "document type account amount", by
// the journal's list of entries by trading partner.
func (e env) tradingPartnerLines(t *testing.T) string {
	t.Helper()
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries?legal_entity_id=%s&trading_partner_id=%s&order_by=posting_date", e.DE, e.US), "", "")
	numbers := map[string]string{}
	for _, n := range []string{"100000", "140000", "211000", "225000", "520000"} {
		numbers[e.Account(t, "2000", n).String()] = n
	}
	var out []string
	for _, it := range apitest.List(t, page, "items") {
		en := it.(map[string]any)
		for _, l := range en["lines"].([]any) {
			m := l.(map[string]any)
			tp := "-"
			if p, ok := m["trading_partner"].(map[string]any); ok {
				tp = fmt.Sprint(p["code"], " ", p["name"])
			}
			out = append(out, fmt.Sprintf("%s %s %s %s", en["document_type"], numbers[m["account_id"].(string)], m["amount"].(map[string]any)["amount"], tp))
		}
	}
	return strings.Join(out, "; ")
}

// consolidate runs group ACME's consolidation of November 2026 as its
// workflow does and returns its intercompany differences and the lines of
// its elimination entries.
func (e env) consolidate(t *testing.T) (differences, entries string) {
	t.Helper()
	o, _ := e.Must(202, "POST", "/v1/consolidation-runs", `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
	run := uuid.MustParse(strings.TrimPrefix(o["resource"].(string), "/v1/consolidation-runs/"))
	h := app.Consolidation(app.Config{})
	step := func(name string, fn func(ctx context.Context) error) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
			t.Fatalf("consolidation run, %s: %v", name, err)
		}
	}
	var companies []uuid.UUID
	step("prepare", func(ctx context.Context) (err error) {
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	})
	for _, c := range companies {
		step("translate", func(ctx context.Context) error { return h.TranslateMember(ctx, run, c) })
	}
	step("eliminate", func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
	step("complete", func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) })
	money := func(v any) string {
		if v == nil {
			return "-"
		}
		return v.(map[string]any)["amount"].(string)
	}
	page, _ := e.Must(200, "GET", "/v1/consolidation-runs/"+run.String()+"/differences", "", "")
	var ds, es []string
	for _, it := range apitest.List(t, page, "items") {
		d := it.(map[string]any)
		partner := "-"
		if p, ok := d["partner"].(map[string]any); ok {
			partner = fmt.Sprint(p["code"])
		}
		ds = append(ds, fmt.Sprintf("%s %s/%s %s: %s + %s = %s", d["elimination"].(map[string]any)["code"], d["company_code"], partner, d["status"],
			money(d["amount"]), money(d["partner_amount"]), money(d["difference"])))
	}
	page, _ = e.Must(200, "GET", "/v1/consolidation-runs/"+run.String()+"/entries", "", "")
	for _, it := range apitest.List(t, page, "items") {
		for _, l := range it.(map[string]any)["lines"].([]any) {
			m := l.(map[string]any)
			tp := "-"
			if p, ok := m["trading_partner"].(map[string]any); ok {
				tp = fmt.Sprint(p["code"])
			}
			es = append(es, fmt.Sprintf("%s %v tp %s %s", m["group_account"].(map[string]any)["number"], m["company_code"], tp, money(m["amount"])))
		}
	}
	return strings.Join(ds, "; "), strings.Join(es, "; ")
}

// TestTradingPartnerMigrated: a vendor whose SAP master names a company of
// the group as its trading partner (LFA1-VBUND, the SAP specialist's A2)
// loads linked to that company's legal entity, so the ledger gives every
// line of its documents the trading partner, the expense and tax lines
// SAP left without one included, and its clearing's lines theirs; the
// ordinary vendor's lines get none. The reconciliation agrees with SAP on
// the open payables by trading partner, a consolidation run matches the
// migrated payable with the other company's receivable and eliminates
// both, and the same extract again loads nothing and changes no line
// (ADR-0059 §4 and As built).
func TestTradingPartnerMigrated(t *testing.T) {
	e := intercompanySetup(t)
	write(t, e.dir, "ic.json", intercompany())
	c := e.connector(t, "ic.json")
	_, got := e.act(t, e.run(t, c, "documents", false, e.DE.String()), "start")
	if got["status"] != "reconciled" && got["status"] != "loaded" {
		t.Fatalf("status: got %v, want the run loaded: %v", got["status"], got["report"])
	}
	_, got = e.act(t, got, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("status: got %v, want reconciled: %v", got["status"], got["report"])
	}
	report := got["report"].(map[string]any)
	if n := len(report["quality"].([]any)); n != 0 {
		t.Errorf("quality findings: got %v, want none", report["quality"])
	}
	if got, want := partnerChecks(report), "2000 Open payables by trading partner total: -90.00 EUR / -90.00 EUR true; "+
		"2000 Open payables by trading partner: trading partners with differences: 0 / 0 true; "+
		"2000 Account balances by trading partner total: 90.00 EUR / 90.00 EUR true; "+
		"2000 Account balances by trading partner: balances with differences: 0 / 0 true"; got != want {
		t.Errorf("the reconciliation's checks by trading partner:\ngot  %s\nwant %s", got, want)
	}
	var linked string
	pgtest.Scan(t, `SELECT string_agg(p.name || ' ' || coalesce(le.code, '-'), ', ' ORDER BY p.name) FROM partner.business_partner p
		LEFT JOIN tenancy.legal_entity le ON le.tenant_id = p.tenant_id AND le.id = p.group_company_id
		WHERE p.tenant_id = $1 AND p.name IN ('Acme Corp US', 'Tiny GmbH')`, []any{e.Tenant.ID}, &linked)
	if want := "Acme Corp US 1000, Tiny GmbH -"; linked != want {
		t.Errorf("the suppliers' group companies: got %s, want %s", linked, want)
	}
	const tp = "1000 Acme Corp US"
	lines := "RE 520000 75.00 " + tp + "; RE 140000 15.00 " + tp + "; RE 225000 -90.00 " + tp + "; RE 520000 40.00 " + tp + "; RE 225000 -40.00 " + tp +
		"; KZ 225000 40.00 " + tp + "; KZ 100000 -40.00 " + tp
	if got := e.tradingPartnerLines(t); got != lines {
		t.Errorf("company 2000's lines naming company 1000:\ngot  %s\nwant %s", got, lines)
	}
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-11-05", "posting_date": "2026-11-05",
		"currency": "USD", "lines": [{"account_id": %q, "amount": {"amount": "100.00", "currency": "USD"}, "trading_partner_id": %q},
		{"account_id": %q, "amount": {"amount": "-100.00", "currency": "USD"}}]}`, e.US, e.Account(t, "1000", "125000"), e.DE, e.Account(t, "1000", "100000")), "")
	differences, entries := e.consolidate(t)
	if want := "IC_BALANCES 1000/2000 matched: 90.00 + -90.00 = 0.00"; differences != want {
		t.Errorf("the consolidation run's intercompany differences:\ngot  %s\nwant %s", differences, want)
	}
	if want := "G1250 1000 tp 2000 -90.00; G2250 2000 tp 1000 90.00"; !strings.HasPrefix(entries, want) {
		t.Errorf("the elimination's lines:\ngot  %s\nwant them to start with %s", entries, want)
	}
	entriesBefore := e.entries(t)
	_, again := e.act(t, e.run(t, c, "documents", false, e.DE.String()), "start")
	if again["status"] != "loaded" {
		t.Fatalf("the same extract again: got %v, want loaded: %v", again["status"], again["report"])
	}
	if loaded := again["report"].(map[string]any)["loaded"].(map[string]any); len(loaded) != 0 {
		t.Errorf("loaded by the same extract again: got %v, want nothing", loaded)
	}
	if n := e.entries(t); n != entriesBefore {
		t.Errorf("journal entries after the same extract again: got %d, want %d", n, entriesBefore)
	}
	if got := e.tradingPartnerLines(t); got != lines {
		t.Errorf("the lines naming company 1000 after the same extract again:\ngot  %s\nwant %s", got, lines)
	}
}

// TestTradingPartnerInBalancesMode: an opening entry keeps each open
// vendor item with its supplier, so the intercompany vendor's item carries
// its trading partner and reconciles by it, while the account balances
// and the ordinary vendor's item carry none: an opening entry's vendors
// name different trading partners, so nothing is given to its other lines
// (ADR-0059 §4).
func TestTradingPartnerInBalancesMode(t *testing.T) {
	e := intercompanySetup(t)
	write(t, e.dir, "ic.json", intercompany())
	c := e.connector(t, "ic.json")
	_, got := e.act(t, e.run(t, c, "balances", true, e.DE.String()), "start")
	if got["status"] != "reconciled" {
		t.Fatalf("rehearsal: got %v, want reconciled: %v", got["status"], got["report"])
	}
	if got, want := partnerChecks(got["report"].(map[string]any)), "2000 Open payables by trading partner total: -90.00 EUR / -90.00 EUR true; "+
		"2000 Open payables by trading partner: trading partners with differences: 0 / 0 true"; got != want {
		t.Errorf("the rehearsal's checks by trading partner:\ngot  %s\nwant %s", got, want)
	}
	if _, got = e.act(t, e.run(t, c, "balances", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("load: got %v, want loaded: %v", got["status"], got["report"])
	}
	var lines string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_lc || ' ' || coalesce(le.code, '-'), ', ' ORDER BY l.line_no)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		LEFT JOIN tenancy.legal_entity le ON le.tenant_id = l.tenant_id AND le.id = l.trading_partner_id
		WHERE l.tenant_id = $1 AND l.partner_id IS NOT NULL`, []any{e.Tenant.ID}, &lines)
	if want := "225000 -9000 1000, 211000 -3000 -"; lines != want {
		t.Errorf("the opening entry's open items: got %s, want %s", lines, want)
	}
	if n := count(t, `SELECT count(*) FROM ledger.journal_line WHERE tenant_id = $1 AND partner_id IS NULL AND trading_partner_id IS NOT NULL`, e.Tenant.ID); n != 0 {
		t.Errorf("account balance lines with a trading partner: got %d, want none", n)
	}
}

// TestTradingPartnerInAnOpeningEntryOfIntercompanyItemsAlone: when the
// only open vendor items at cutover are the intercompany vendor's, every
// partner line of the opening entry names the same company. The entry
// gathers unrelated lines (ledger.Posting.Collective), so its account
// balances still take no trading partner: bank, expenses and tax are no
// balances with the other company.
func TestTradingPartnerInAnOpeningEntryOfIntercompanyItemsAlone(t *testing.T) {
	e := intercompanySetup(t)
	x := intercompany()
	x.ACDOCA = x.ACDOCA[:7] // without the ordinary vendor's open invoice
	write(t, e.dir, "ic.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "ic.json"), "balances", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("load: got %v, want loaded: %v", got["status"], got["report"])
	}
	var lines string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_lc || ' ' || coalesce(le.code, '-'), ', ' ORDER BY l.line_no)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		LEFT JOIN tenancy.legal_entity le ON le.tenant_id = l.tenant_id AND le.id = l.trading_partner_id
		WHERE l.tenant_id = $1`, []any{e.Tenant.ID}, &lines)
	if want := "100000 -4000 -, 140000 1500 -, 520000 11500 -, 225000 -9000 1000"; lines != want {
		t.Errorf("the opening entry's lines: got %s, want %s", lines, want)
	}
}

// TestTradingPartnerNotOnThePlatform: a vendor whose trading partner is a
// company that is no legal entity of the tenant (or its own company) does
// not stop the run. The gate warns of the first, the supplier loads linked
// to no company and its lines without a trading partner; one that is the
// vendor's own company loads linked to it and its lines in that company
// carry none, being no intercompany lines; the reconciliation shows no
// check by trading partner, neither side having any.
func TestTradingPartnerNotOnThePlatform(t *testing.T) {
	for _, c := range []struct {
		name, vbund, company, group string
		warnings                    int
	}{
		{"no legal entity", "003000", "3000", "-", 1},
		{"the vendor's own company", "002000", "", "2000", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := intercompanySetup(t)
			x := intercompany()
			x.LFA1[1].VBUND = c.vbund
			if c.company != "" {
				x.T001 = append(x.T001, sap.T001{BUKRS: c.company, BUTXT: "Maple Industries Canada", WAERS: "CAD", RCOMP: c.vbund})
			}
			for i := range x.ACDOCA {
				if x.ACDOCA[i].RASSC != "" {
					x.ACDOCA[i].RASSC = c.vbund
				}
			}
			write(t, e.dir, "ic.json", x)
			_, got := e.act(t, e.run(t, e.connector(t, "ic.json"), "documents", false, e.DE.String()), "start")
			if got["status"] != "loaded" {
				t.Fatalf("status: got %v, want loaded: %v", got["status"], got["report"])
			}
			var findings []string
			for _, i := range got["report"].(map[string]any)["quality"].([]any) {
				m := i.(map[string]any)
				findings = append(findings, fmt.Sprintf("%s %s %s: %s", m["severity"], m["table"], m["key"], m["message"]))
			}
			if len(findings) != c.warnings {
				t.Fatalf("quality findings: got %v, want %d", findings, c.warnings)
			}
			if want := "warning LFA1 " + intercompanyVendor + `: vendor "Acme Corp US" names trading partner 003000 (LFA1-VBUND), which is company code ` +
				"3000, which is no legal entity of the tenant: the supplier loads linked to no company of the group and its documents without a trading " +
				"partner"; c.warnings == 1 && findings[0] != want {
				t.Errorf("the warning:\ngot  %s\nwant %s", findings[0], want)
			}
			var group string
			pgtest.Scan(t, `SELECT coalesce((SELECT le.code FROM tenancy.legal_entity le WHERE le.tenant_id = p.tenant_id AND le.id = p.group_company_id), '-')
				FROM partner.business_partner p WHERE p.tenant_id = $1 AND p.name = 'Acme Corp US'`, []any{e.Tenant.ID}, &group)
			if group != c.group {
				t.Errorf("the supplier's group company: got %s, want %s", group, c.group)
			}
			if n := count(t, `SELECT count(*) FROM ledger.journal_line WHERE tenant_id = $1 AND trading_partner_id IS NOT NULL`, e.Tenant.ID); n != 0 {
				t.Errorf("lines with a trading partner: got %d, want none", n)
			}
			_, got = e.act(t, got, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconciliation: got %v, want reconciled: %v", got["status"], got["report"])
			}
			if checks := partnerChecks(got["report"].(map[string]any)); checks != "" {
				t.Errorf("checks by trading partner: got %s, want none", checks)
			}
		})
	}
}

// TestTradingPartnerOfAnEarlierRunsSupplier: a later run does not link a
// supplier an earlier run loaded: a supplier's group company changes
// through a partner change with its approval. So when SAP's vendor master
// names a trading partner only after the first run, the next run's
// documents load without one, and its reconciliation says so: SAP's open
// payables and its documents' other lines under the trading partner
// against none on the platform.
func TestTradingPartnerOfAnEarlierRunsSupplier(t *testing.T) {
	e := intercompanySetup(t)
	first := intercompany()
	first.LFA1[1].VBUND = ""
	for i := range first.ACDOCA {
		first.ACDOCA[i].RASSC = ""
	}
	first.ACDOCA = first.ACDOCA[3:] // without the open intercompany invoice
	write(t, e.dir, "first.json", first)
	if _, got := e.act(t, e.run(t, e.connector(t, "first.json"), "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	write(t, e.dir, "ic.json", intercompany())
	_, got := e.act(t, e.run(t, e.connector(t, "ic.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("second run: got %v, want loaded: %v", got["status"], got["report"])
	}
	_, got = e.act(t, got, "reconcile")
	if got["status"] != "failed" {
		t.Fatalf("reconciliation: got %v, want failed: %v", got["status"], got["report"])
	}
	report := got["report"].(map[string]any)
	if got, want := partnerChecks(report), "2000 Open payables by trading partner total: -90.00 EUR / 0.00 EUR false; "+
		"2000 Open payables by trading partner: trading partners with differences: 0 / 1 false; "+
		"2000 Account balances by trading partner total: 90.00 EUR / 0.00 EUR false; "+
		"2000 Account balances by trading partner: balances with differences: 0 / 3 false"; got != want {
		t.Errorf("the checks by trading partner:\ngot  %s\nwant %s", got, want)
	}
	var diffs []string
	for _, d := range report["diffs"].([]any) {
		m := d.(map[string]any)
		diffs = append(diffs, fmt.Sprintf("%s %s: %s / %s", m["company_code"], m["key"], m["sap"], m["platform"]))
	}
	if got, want := strings.Join(diffs, "; "), "2000 trading partner 1000: -90.00 EUR / 0.00 EUR; "+
		"2000 account 100000 trading partner 1000: -40.00 EUR / 0.00 EUR; 2000 account 140000 trading partner 1000: 15.00 EUR / 0.00 EUR; "+
		"2000 account 520000 trading partner 1000: 115.00 EUR / 0.00 EUR"; got != want {
		t.Errorf("the differences:\ngot  %s\nwant %s", got, want)
	}
}

// withoutVendorLine is intercompany with two documents that have no
// vendor line (po:2026-11-10): a recharge from company 1000 posted
// between G/L accounts, 60.00, whose expense line names the trading
// partner itself (ACDOCA-RASSC 001000) and whose bank line names none;
// and one of 20.00 whose expense line names company ID 009000, which no
// company code of the extract is assigned to.
func withoutVendorLine() *sap.Extract {
	x := intercompany()
	line := func(belnr, docln, account, hsl, rassc string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, WSL: hsl,
			BUDAT: "20261106", BLART: "SA", RASSC: rassc}
	}
	x.ACDOCA = append(x.ACDOCA, line("0100000005", "000001", "0000520000", "60.00", "001000"), line("0100000005", "000002", "0000100000", "-60.00", ""),
		line("0100000006", "000001", "0000520000", "20.00", "009000"), line("0100000006", "000002", "0000100000", "-20.00", ""))
	return x
}

// TestTradingPartnerFromTheLine: in documents mode a line the load posts
// without a supplier carries the trading partner it names itself
// (ACDOCA-RASSC; po:2026-11-10), so an intercompany document without a
// vendor line arrives with its partner, on the line that names it alone;
// a line naming a company that is no legal entity loads without one and
// is warned of, per company and trading partner; the documents with a
// vendor line keep theirs as before. The reconciliation proves the
// balances per account and trading partner, and the same extract again
// loads nothing. The same extract without RASSC loads as it did before:
// the vendor's documents carry the trading partner of its master, the
// documents without a vendor line none, and nothing is warned of.
func TestTradingPartnerFromTheLine(t *testing.T) {
	const tp = "1000 Acme Corp US"
	vendors := "RE 520000 75.00 " + tp + "; RE 140000 15.00 " + tp + "; RE 225000 -90.00 " + tp + "; RE 520000 40.00 " + tp + "; RE 225000 -40.00 " + tp +
		"; KZ 225000 40.00 " + tp + "; KZ 100000 -40.00 " + tp
	for _, c := range []struct {
		name            string
		change          func(x *sap.Extract)
		warnings        []string
		lines, balances string
	}{
		{"with RASSC", func(*sap.Extract) {}, []string{"warning ACDOCA 2000/009000: journal lines of company 2000 name trading partner 009000 " +
			"(ACDOCA-RASSC), which is the company ID of no company code in the extract (T001-RCOMP): lines 1, documents 1. They load without a trading " +
			"partner of their own: with the one their document's vendor lines share, or without one where it has none or those name different ones, " +
			"and a consolidation run lists a balance without one as no_trading_partner"},
			vendors + "; SA 520000 60.00 " + tp + "; SA 100000 -60.00 -", "150.00"},
		{"without RASSC", func(x *sap.Extract) {
			for i := range x.ACDOCA {
				x.ACDOCA[i].RASSC = ""
			}
		}, nil, vendors, "90.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := intercompanySetup(t)
			x := withoutVendorLine()
			c.change(x)
			write(t, e.dir, "ic.json", x)
			conn := e.connector(t, "ic.json")
			_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
			if got["status"] != "loaded" {
				t.Fatalf("status: got %v, want loaded: %v", got["status"], got["report"])
			}
			var findings []string
			for _, i := range got["report"].(map[string]any)["quality"].([]any) {
				m := i.(map[string]any)
				findings = append(findings, fmt.Sprintf("%s %s %s: %s", m["severity"], m["table"], m["key"], m["message"]))
			}
			if got, want := strings.Join(findings, "\n"), strings.Join(c.warnings, "\n"); got != want {
				t.Errorf("quality findings:\ngot  %s\nwant %s", got, want)
			}
			if got := e.tradingPartnerLines(t); got != c.lines {
				t.Errorf("company 2000's entries naming company 1000:\ngot  %s\nwant %s", got, c.lines)
			}
			_, got = e.act(t, got, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconciliation: got %v, want reconciled: %v", got["status"], got["report"])
			}
			if got, want := partnerChecks(got["report"].(map[string]any)), "2000 Open payables by trading partner total: -90.00 EUR / -90.00 EUR true; "+
				"2000 Open payables by trading partner: trading partners with differences: 0 / 0 true; "+
				"2000 Account balances by trading partner total: "+c.balances+" EUR / "+c.balances+" EUR true; "+
				"2000 Account balances by trading partner: balances with differences: 0 / 0 true"; got != want {
				t.Errorf("the reconciliation's checks by trading partner:\ngot  %s\nwant %s", got, want)
			}
			entriesBefore := e.entries(t)
			_, again := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
			if again["status"] != "loaded" {
				t.Fatalf("the same extract again: got %v, want loaded: %v", again["status"], again["report"])
			}
			if n := e.entries(t); n != entriesBefore {
				t.Errorf("journal entries after the same extract again: got %d, want %d", n, entriesBefore)
			}
			if got := e.tradingPartnerLines(t); got != c.lines {
				t.Errorf("the entries naming company 1000 after the same extract again:\ngot  %s\nwant %s", got, c.lines)
			}
		})
	}
}
