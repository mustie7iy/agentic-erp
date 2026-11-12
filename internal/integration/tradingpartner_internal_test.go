package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// codeEntities is the legal entities port of a trading partner test: the
// tenant's legal entities by company code, or err.
type codeEntities struct {
	LegalEntities
	byCode map[string]uuid.UUID
	err    error
}

func (c codeEntities) EntityByCode(_ context.Context, code string) (tenancy.Entity, bool, error) {
	id, ok := c.byCode[code]
	return tenancy.Entity{ID: id, Code: code}, ok, c.err
}

// TestGroupCompanies: a vendor's trading partner (LFA1-VBUND) and a
// journal line's (ACDOCA-RASSC) are a company
// ID, which names the legal entity of the one company code assigned to it
// (T001-RCOMP), compared without blanks and ALPHA zeros; one assigned to
// no company code, to several, or to a company code that is no legal
// entity of the tenant names none, with why. Each is resolved once, and a
// failed read fails the run.
func TestGroupCompanies(t *testing.T) {
	us := uuid.New()
	ca := uuid.New()
	e := &Endpoints{LegalEntities: codeEntities{byCode: map[string]uuid.UUID{"1000": us, "7000": ca}}}
	x := &sap.Extract{
		LFA1: []sap.LFA1{{LIFNR: "1", VBUND: "001000"}, {LIFNR: "2", VBUND: " 1000 "}, {LIFNR: "3"}, {LIFNR: "4", VBUND: "004000"},
			{LIFNR: "5", VBUND: "005000"}, {LIFNR: "6", VBUND: "006000"}},
		ACDOCA:       []sap.ACDOCA{{RASSC: "001000"}, {}, {RASSC: " 7000"}, {RASSC: "007000"}},
		CompanyCodes: map[string][]string{"1000": {"1000"}, "5000": {"5000", "5100"}, "6000": {"6000"}, "7000": {"7000"}},
	}
	got, err := e.groupCompanies(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]groupCompany{
		"1000": {code: "1000", id: us},
		"4000": {why: "is the company ID of no company code in the extract (T001-RCOMP)"},
		"5000": {why: "is the company ID of several company codes (5000, 5100), which no one legal entity is"},
		"6000": {code: "6000", why: "is company code 6000, which is no legal entity of the tenant"},
		"7000": {code: "7000", id: ca},
	}
	if len(got) != len(want) {
		t.Errorf("trading partners resolved: got %d (%v), want %d", len(got), got, len(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("trading partner %s: got %+v, want %+v", id, got[id], w)
		}
	}
	tg := target{partners: got}
	for lifnr, w := range map[string]*uuid.UUID{"1": &us, "2": &us, "3": nil, "4": nil, "5": nil, "6": nil} {
		g := tg.groupCompanyOf(x.LFA1[int(lifnr[0]-'1')])
		if (g == nil) != (w == nil) || g != nil && *g != *w {
			t.Errorf("group company of vendor %s: got %v, want %v", lifnr, g, w)
		}
	}
	down := errors.New("down")
	e.LegalEntities = codeEntities{err: down}
	if _, err := e.groupCompanies(context.Background(), x); !errors.Is(err, down) || !strings.Contains(err.Error(), "trading partner 001000 (LFA1-VBUND)") {
		t.Errorf("a failed read of a vendor's legal entity: got %v, want it to fail the run naming the vendor's field", err)
	}
	x.LFA1 = nil
	if _, err := e.groupCompanies(context.Background(), x); !errors.Is(err, down) || !strings.Contains(err.Error(), "trading partner 001000 (ACDOCA-RASSC)") {
		t.Errorf("a failed read of a line's legal entity: got %v, want it to fail the run naming the line's field", err)
	}
}

// TestOwnPartner: a line's own trading partner (ACDOCA-RASSC) is the
// legal entity its company ID names, which a documents run posts a line
// without a supplier with; one that names none, no legal entity, a
// company ID the run did not resolve or the line's own company is not
// taken, with why.
func TestOwnPartner(t *testing.T) {
	us, de := uuid.New(), uuid.New()
	tg := target{entities: map[string]tenancy.Entity{"2000": {ID: de}}, partners: map[string]groupCompany{"1000": {code: "1000", id: us},
		"2000": {code: "2000", id: de}, "6000": {code: "6000", why: "is company code 6000, which is no legal entity of the tenant"}}}
	for _, c := range []struct {
		name, rassc string
		want        groupCompany
		ok          bool
	}{
		{"a legal entity", " 001000", groupCompany{code: "1000", id: us}, true},
		{"none", "  ", groupCompany{}, false},
		{"no legal entity", "006000", tg.partners["6000"], false},
		{"not resolved", "009000", groupCompany{why: noCompanyCode}, false},
		{"the line's own company", "002000", groupCompany{code: "2000", why: "is the line's own company, which is no trading partner of its own lines"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := tg.ownPartner(sap.ACDOCA{RBUKRS: "2000", RASSC: c.rassc})
			if got != c.want || ok != c.ok {
				t.Errorf("own partner: got %+v %t, want %+v %t", got, ok, c.want, c.ok)
			}
		})
	}
	if !tg.takesOwn() {
		t.Error("a documents run: got it not taking a line's own trading partner, want it taken")
	}
	tg.balances = true
	if tg.takesOwn() {
		t.Error("a balances run: got it taking a line's own trading partner, want its lines loaded as before")
	}
}

// tpLine is a journal line of company 2000 for the trading partner tests.
func tpLine(belnr, docln, account, hsl, lifnr, rassc string) sap.ACDOCA {
	return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, RWCUR: "EUR", WSL: hsl,
		BUDAT: "20260310", BLART: "KZ", LIFNR: lifnr, RASSC: rassc}
}

// tradingPartnerFindings are the gate's findings about trading partners,
// each a warning.
func tradingPartnerFindings(t *testing.T, e *sap.Extract, tg target) []string {
	t.Helper()
	var got []string
	for _, i := range checkQuality(e, tg) {
		if strings.Contains(i.Message, "trading partner") {
			if i.Severity != SevWarning {
				t.Errorf("finding %s %s: got severity %s, want a warning", i.Table, i.Key, i.Severity)
			}
			got = append(got, i.Table+" "+i.Key+": "+i.Message)
		}
	}
	return got
}

// TestTradingPartnersReported: the quality gate warns, and never refuses,
// where the load does not take SAP's trading partner over: a vendor whose
// trading partner names no legal entity; vendor lines whose own trading
// partner (ACDOCA-RASSC) is not the vendor's master's, per company and
// vendor; and lines the load posts without a supplier that name one the
// load cannot give them, per company and trading partner: no legal
// entity of the tenant, or the line's own company. A line naming a legal
// entity loads with it, in a document with a vendor line or without one,
// and is not reported; nor is a vendor line carrying its master's, a line
// SAP left without one, or anything when no line carries a trading
// partner, as an extract without the field has it. A balances run reports
// its lines as before: those carrying one in documents without a vendor
// line, and those carrying another than their document's vendor lines
// give them, per company.
func TestTradingPartnersReported(t *testing.T) {
	linked := map[string]groupCompany{"1000": {code: "1000", id: uuid.New()}}
	unknown := map[string]groupCompany{"1000": {code: "1000", why: "is company code 1000, which is no legal entity of the tenant"}}
	const derived = "They load with the master's: the platform derives a line's trading partner from its business partner"
	both := map[string]groupCompany{"1000": linked["1000"], "3000": {code: "3000", id: uuid.New()}}
	const without = "They load without a trading partner of their own: with the one their document's vendor lines share, or without one where it " +
		"has none or those name different ones, and a consolidation run lists a balance without one as no_trading_partner"
	second := func(e *sap.Extract, _ *target) {
		e.LFA1 = append(e.LFA1, sap.LFA1{LIFNR: "0000000002", NAME1: "Other GmbH", STCD1: "DE2", VBUND: "003000"})
		e.LFB1 = append(e.LFB1, sap.LFB1{LIFNR: "0000000002", BUKRS: "2000", AKONT: "0000211000", ZTERM: "NT30"})
	}
	balances := func(_ *sap.Extract, tg *target) { tg.balances = true }
	alone := []sap.ACDOCA{
		tpLine("0100000050", "000001", "0000170000", "-10.00", "", "001000"), tpLine("0100000050", "000002", "0000100000", "10.00", "", "001000"),
		tpLine("0100000051", "000001", "0000170000", "-5.00", "", "001000"), tpLine("0100000051", "000002", "0000100000", "5.00", "", "")}
	for _, c := range []struct {
		name     string
		vbund    string
		partners map[string]groupCompany
		lines    []sap.ACDOCA
		want     []string
		change   func(e *sap.Extract, tg *target)
	}{
		{name: "other lines naming a company that is no legal entity", vbund: "001000", partners: linked, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "001000"),
			tpLine("0100000050", "000002", "0000100000", "6.00", "", "003000"), tpLine("0100000050", "000003", "0000140000", "4.00", "", "001000"),
			tpLine("0100000051", "000001", "0000211000", "-5.00", "0000000001", "001000"),
			tpLine("0100000051", "000002", "0000100000", "5.00", "0000000001", "003000")}, want: []string{
			"ACDOCA 2000/003000: journal lines of company 2000 name trading partner 003000 (ACDOCA-RASSC), which is the company ID of no company " +
				"code in the extract (T001-RCOMP): lines 2, documents 2. " + without}},
		{name: "other lines naming another legal entity than their document's vendors", vbund: "001000", partners: both, change: second, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-6.00", "0000000001", "001000"),
			tpLine("0100000050", "000002", "0000211000", "-4.00", "0000000002", "003000"),
			tpLine("0100000050", "000003", "0000211000", "-1.00", "0000000001", "001000"),
			tpLine("0100000050", "000004", "0000100000", "11.00", "", "003000")}},
		{name: "other lines naming their own company", partners: map[string]groupCompany{"2000": {code: "2000"}}, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000170000", "-10.00", "", "002000"), tpLine("0100000050", "000002", "0000100000", "10.00", "", "")},
			want: []string{"ACDOCA 2000/002000: journal lines of company 2000 name trading partner 002000 (ACDOCA-RASSC), which is the line's own " +
				"company, which is no trading partner of its own lines: lines 1, documents 1. " + without}},
		{name: "other lines of a document in an opening entry", vbund: "001000", partners: linked, change: balances, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "001000"),
			tpLine("0100000050", "000002", "0000100000", "10.00", "", "001000")}, want: []string{
			"ACDOCA 2000: journal lines of company 2000 that carry another trading partner (ACDOCA-RASSC) than their document's vendor lines give " +
				"them: lines 1, documents 1. They load without one in the opening entry, whose account balances take none whatever its vendor " +
				"items name; in a document that posts on its own (a down payment's, an item in a foreign currency) they load with the trading partner " +
				"its vendors share, or without one where those name different ones, none or a company that is no legal entity"}},
		{name: "none anywhere", lines: []sap.ACDOCA{tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", ""),
			tpLine("0100000050", "000002", "0000100000", "10.00", "", "")}},
		{name: "the vendor's on its line, none or the same on the others", vbund: "001000", partners: linked, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "001000"),
			tpLine("0100000050", "000002", "0000100000", "6.00", "", ""), tpLine("0100000050", "000003", "0000140000", "4.00", "", "1000")}},
		{name: "a master without lines' trading partners in the extract", vbund: "001000", partners: linked, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", ""), tpLine("0100000050", "000002", "0000100000", "10.00", "", "")}},
		{name: "a trading partner that is no legal entity", vbund: "001000", partners: unknown, want: []string{
			`LFA1 0000000001: vendor "Tiny GmbH" names trading partner 001000 (LFA1-VBUND), which is company code 1000, which is no legal entity of ` +
				"the tenant: the supplier loads linked to no company of the group and its documents without a trading partner"}},
		{name: "vendor lines with another one than the master's", vbund: "001000", partners: both, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "003000"),
			tpLine("0100000050", "000002", "0000100000", "10.00", "", ""),
			tpLine("0100000051", "000001", "0000211000", "-5.00", "0000000001", ""),
			tpLine("0100000051", "000002", "0000100000", "5.00", "", ""),
			tpLine("0100000052", "000001", "0000211000", "-5.00", "0000000001", "003000"),
			tpLine("0100000052", "000002", "0000100000", "5.00", "", ""),
			tpLine("0100000053", "000001", "0000211000", "-5.00", "0000000001", "001000"),
			tpLine("0100000053", "000002", "0000100000", "5.00", "", "")}, want: []string{
			`ACDOCA 2000/0000000001: vendor lines of vendor 0000000001 "Tiny GmbH" in company 2000 carry another trading partner (ACDOCA-RASSC) ` +
				"than the vendor's master (LFA1-VBUND 001000): lines 3 (none on 1, 003000 on 2). " + derived}},
		{name: "vendor lines with one where the master has none", partners: linked, lines: []sap.ACDOCA{
			tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "001000"),
			tpLine("0100000050", "000002", "0000100000", "10.00", "", "")}, want: []string{
			`ACDOCA 2000/0000000001: vendor lines of vendor 0000000001 "Tiny GmbH" in company 2000 carry another trading partner (ACDOCA-RASSC) ` +
				"than the vendor's master (LFA1-VBUND none): lines 1 (001000 on 1). " + derived}},
		{name: "lines of documents without a vendor line naming a legal entity", partners: linked, lines: alone},
		{name: "lines of documents without a vendor line naming none", partners: unknown, lines: alone, want: []string{
			"ACDOCA 2000/001000: journal lines of company 2000 name trading partner 001000 (ACDOCA-RASSC), which is company code 1000, which is no " +
				"legal entity of the tenant: lines 3, documents 2. " + without}},
		{name: "lines of documents without a vendor line in a balances run", partners: linked, change: balances, lines: alone, want: []string{
			"ACDOCA 2000: journal lines of company 2000 that carry a trading partner (ACDOCA-RASSC) in documents without a vendor line load without " +
				"one: lines 3, documents 2. A balances run derives a document's trading partner from its vendor line, so a consolidation run lists " +
				"their balances as no_trading_partner until they are reposted with one"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			e.LFA1[0].VBUND, e.ACDOCA, tg.partners = c.vbund, c.lines, c.partners
			if c.change != nil {
				c.change(e, &tg)
			}
			if got := tradingPartnerFindings(t, e, tg); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("trading partners reported:\ngot  %s\nwant %s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// TestExtractWithoutTradingPartnersReported: an extract that cannot carry
// trading partners, as an OData one, says so once in the report when it
// holds vendors, so its intercompany suppliers do not load unlinked
// without a word.
func TestExtractWithoutTradingPartnersReported(t *testing.T) {
	for _, c := range []struct {
		name    string
		vendors bool
		want    int
	}{{"with vendors", true, 1}, {"without vendors", false, 0}} {
		t.Run(c.name, func(t *testing.T) {
			e, tg := dpExtract(), dpTarget()
			e.NoTradingPartners = true
			if !c.vendors {
				e.LFA1, e.LFB1, e.ACDOCA = nil, nil, nil
			}
			got := tradingPartnerFindings(t, e, tg)
			if len(got) != c.want {
				t.Fatalf("findings: got %v, want %d", got, c.want)
			}
			if c.want == 1 && !strings.HasPrefix(got[0], "LFA1 VBUND: the extract carries no trading partner: the OData mapping reads neither LFA1-VBUND nor ACDOCA-RASSC.") {
				t.Errorf("finding: got %s, want it to name the fields the extract lacks", got[0])
			}
		})
	}
}

// partnerPayables is the reports port of a reconciler test: it reports
// open payables per trading partner, or err.
type partnerPayables struct {
	Reports
	open map[uuid.UUID]money.Minor
	err  error
}

func (r partnerPayables) OpenPayablesByTradingPartner(context.Context, uuid.UUID, int16) (map[uuid.UUID]money.Minor, error) {
	return r.open, r.err
}

// TestTradingPartnerReconciliation: the open payables per trading partner
// are compared when either side has any, SAP's under the company code of
// the vendors' trading partner, the platform's under the same, or under
// its id alone for a legal entity no vendor of the extract names; an open
// item of a vendor whose trading partner is the company itself, names no
// legal entity or is none counts for no trading partner; a failed read
// fails the reconciliation.
func TestTradingPartnerReconciliation(t *testing.T) {
	// other sorts after "trading partner 1000" whatever the ids drawn: the
	// differences come in key order.
	us, de, other := uuid.New(), uuid.New(), uuid.MustParse("f0000000-0000-7000-8000-000000000001")
	tg := target{year: 2026, partners: map[string]groupCompany{"1000": {code: "1000", id: us}, "2000": {code: "2000", id: de},
		"6000": {code: "6000", why: "is company code 6000, which is no legal entity of the tenant"}}}
	s := companyScope{cc: "2000", le: de, cur: "EUR", format: func(v money.Minor) string { return v.Plain(2) + " EUR" }}
	reconcilerOf := func(open map[uuid.UUID]money.Minor, err error) *reconciler {
		return &reconciler{e: &Endpoints{Reports: partnerPayables{open: open, err: err}}, t: tg,
			partnerOf: map[string]string{"1": "1000", "2": "2000", "3": "6000", "4": ""}}
	}
	rc := reconcilerOf(nil, nil)
	for lifnr, want := range map[string]string{"1": "trading partner 1000", "2": "", "3": "", "4": "", "5": ""} {
		if got, ok := rc.sapTradingPartner(s, lifnr); got != want || ok != (want != "") {
			t.Errorf("SAP's trading partner of vendor %s's items: got %q %t, want %q", lifnr, got, ok, want)
		}
	}
	if err := rc.tradingPartners(context.Background(), s, nil); err != nil || len(rc.rec.Checks) != 0 {
		t.Errorf("neither side with any: got %v and %v, want no check", err, rc.rec.Checks)
	}
	rc = reconcilerOf(map[uuid.UUID]money.Minor{us: -9000}, nil)
	if err := rc.tradingPartners(context.Background(), s, map[string]money.Minor{"trading partner 1000": -9000}); err != nil {
		t.Fatal(err)
	}
	if got, want := checkLines(rc.rec), "Open payables by trading partner total -90.00 EUR -90.00 EUR true; "+
		"Open payables by trading partner: trading partners with differences 0 0 true"; got != want {
		t.Errorf("agreeing sides:\ngot  %s\nwant %s", got, want)
	}
	rc = reconcilerOf(map[uuid.UUID]money.Minor{us: -4000, other: -100}, nil)
	if err := rc.tradingPartners(context.Background(), s, map[string]money.Minor{"trading partner 1000": -9000}); err != nil {
		t.Fatal(err)
	}
	if got, want := checkLines(rc.rec), "Open payables by trading partner total -90.00 EUR -41.00 EUR false; "+
		"Open payables by trading partner: trading partners with differences 0 2 false"; got != want {
		t.Errorf("differing sides:\ngot  %s\nwant %s", got, want)
	}
	var diffs []string
	for _, d := range rc.rec.Diffs {
		diffs = append(diffs, d.CompanyCode+" "+d.Key+": "+d.SAP+" / "+d.Platform)
	}
	if got, want := strings.Join(diffs, "; "), "2000 trading partner 1000: -90.00 EUR / -40.00 EUR; 2000 trading partner "+other.String()+
		": 0.00 EUR / -1.00 EUR"; got != want {
		t.Errorf("differences:\ngot  %s\nwant %s", got, want)
	}
	down := errors.New("down")
	if err := reconcilerOf(nil, down).tradingPartners(context.Background(), s, nil); !errors.Is(err, down) {
		t.Errorf("a failed read of the platform's side: got %v, want it to fail the reconciliation", err)
	}
}

// checkLines renders a reconciliation's checks.
func checkLines(rec Reconciliation) string {
	var out []string
	for _, c := range rec.Checks {
		pass := "false"
		if c.Pass {
			pass = "true"
		}
		out = append(out, c.Name+" "+c.SAP+" "+c.Platform+" "+pass)
	}
	return strings.Join(out, "; ")
}
