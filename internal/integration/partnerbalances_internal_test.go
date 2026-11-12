package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// partnerBalances is the reports port of a reconciler test: it reports
// account balances by trading partner, or err.
type partnerBalances struct {
	Reports
	balances []ledger.PartnerBalance
	err      error
}

func (r partnerBalances) BalancesByTradingPartner(context.Context, uuid.UUID, int16) ([]ledger.PartnerBalance, error) {
	return r.balances, r.err
}

// TestPartnerBalancesReconciliation: the balances by account and trading
// partner of the lines the load posts without a supplier are compared
// when either side has any. SAP's side takes each line under the trading
// partner the load gives it: its own (ACDOCA-RASSC) when that is a legal
// entity other than the company, else the one its document's vendor
// lines share, else none; vendor lines and lines whose amount does not
// parse are left out. The platform's side is keyed by the same account
// and company code, or by the id of a legal entity the extract does not
// name. A balances run does not take a line's own. A failed read fails
// the reconciliation.
func TestPartnerBalancesReconciliation(t *testing.T) {
	// other sorts after "trading partner 1000" whatever the ids drawn: the
	// differences come in key order.
	us, de, ca, other := uuid.New(), uuid.New(), uuid.New(), uuid.MustParse("f0000000-0000-7000-8000-000000000001")
	tg := dpTarget()
	tg.entities = map[string]tenancy.Entity{"2000": {ID: de, Code: "2000", LocalCurrency: "EUR"}}
	tg.partners = map[string]groupCompany{"1000": {code: "1000", id: us}, "2000": {code: "2000", id: de}, "3000": {code: "3000", id: ca},
		"6000": {code: "6000", why: "is company code 6000, which is no legal entity of the tenant"}}
	s := companyScope{cc: "2000", le: de, cur: "EUR", format: func(v money.Minor) string { return v.Plain(2) + " EUR" }}
	x := &sap.Extract{ACDOCA: []sap.ACDOCA{
		tpLine("0100000050", "000001", "0000211000", "-10.00", "0000000001", "001000"), // a vendor line: the open payables' control
		tpLine("0100000050", "000002", "0000100000", "6.00", "", ""),                   // none of its own: its document's
		tpLine("0100000050", "000003", "0000140000", "4.00", "", "003000"),             // its own
		tpLine("0100000051", "000001", "0000170000", "-7.00", "", "001000"),            // no vendor line: its own
		tpLine("0100000051", "000002", "0000100000", "7.00", "", ""),                   // none
		tpLine("0100000052", "000001", "0000170000", "-2.00", "", "006000"),            // no legal entity: none
		tpLine("0100000052", "000002", "0000170000", "1.00", "", "002000"),             // the company itself: none
		tpLine("0100000052", "000003", "0000170000", "x", "", "001000"),                // refused by the journal's control
		{RBUKRS: "9000", RACCT: "0000170000", HSL: "5.00", RASSC: "001000"},            // another company's
	}}
	reconcilerOf := func(balances []ledger.PartnerBalance, err error) *reconciler {
		return &reconciler{e: &Endpoints{Reports: partnerBalances{balances: balances, err: err}}, ext: x, t: tg,
			derived: map[string]string{"2000/2026/0100000050": "1000"}}
	}
	sapSide := "account 100000 trading partner 1000=600 account 140000 trading partner 3000=400 account 170000 trading partner 1000=-700"
	if got := balanceTerms(reconcilerOf(nil, nil).sapPartnerBalances(s)); got != sapSide {
		t.Errorf("SAP's balances by trading partner:\ngot  %s\nwant %s", got, sapSide)
	}
	agreeing := []ledger.PartnerBalance{{Account: "100000", TradingPartner: us, Amount: 600}, {Account: "140000", TradingPartner: ca, Amount: 400},
		{Account: "170000", TradingPartner: us, Amount: -700}}
	rc := reconcilerOf(agreeing, nil)
	if err := rc.partnerBalances(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got, want := checkLines(rc.rec), "Account balances by trading partner total 3.00 EUR 3.00 EUR true; "+
		"Account balances by trading partner: balances with differences 0 0 true"; got != want {
		t.Errorf("agreeing sides:\ngot  %s\nwant %s", got, want)
	}
	rc = reconcilerOf(append(agreeing[:2:2], ledger.PartnerBalance{Account: "170000", TradingPartner: other, Amount: -700}), nil)
	if err := rc.partnerBalances(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got, want := checkLines(rc.rec), "Account balances by trading partner total 3.00 EUR 3.00 EUR true; "+
		"Account balances by trading partner: balances with differences 0 2 false"; got != want {
		t.Errorf("differing sides:\ngot  %s\nwant %s", got, want)
	}
	var diffs []string
	for _, d := range rc.rec.Diffs {
		diffs = append(diffs, d.CompanyCode+" "+d.Key+": "+d.SAP+" / "+d.Platform)
	}
	if got, want := strings.Join(diffs, "; "), "2000 account 170000 trading partner 1000: -7.00 EUR / 0.00 EUR; 2000 account 170000 trading partner "+
		other.String()+": 0.00 EUR / -7.00 EUR"; got != want {
		t.Errorf("differences:\ngot  %s\nwant %s", got, want)
	}
	down := errors.New("down")
	if err := reconcilerOf(nil, down).partnerBalances(context.Background(), s); !errors.Is(err, down) {
		t.Errorf("a failed read of the platform's side: got %v, want it to fail the reconciliation", err)
	}
	x.ACDOCA = x.ACDOCA[4:5]
	rc = reconcilerOf(nil, nil)
	if err := rc.partnerBalances(context.Background(), s); err != nil || len(rc.rec.Checks) != 0 {
		t.Errorf("neither side with any: got %v and %v, want no check", err, rc.rec.Checks)
	}
}

// TestPartnerBalancesOfABalancesRun: a balances run does not take a
// line's own trading partner: SAP's side holds only what a document's
// vendor lines give its other lines.
func TestPartnerBalancesOfABalancesRun(t *testing.T) {
	tg := dpTarget()
	tg.balances = true
	tg.partners = map[string]groupCompany{"1000": {code: "1000", id: uuid.New()}, "3000": {code: "3000", id: uuid.New()}}
	rc := &reconciler{ext: &sap.Extract{ACDOCA: []sap.ACDOCA{
		tpLine("0100000050", "000001", "0000100000", "6.00", "", "003000"), tpLine("0100000051", "000001", "0000170000", "-7.00", "", "001000")}},
		t: tg, derived: map[string]string{"2000/2026/0100000050": "1000"}}
	s := companyScope{cc: "2000", cur: "EUR"}
	if got, want := balanceTerms(rc.sapPartnerBalances(s)), "account 100000 trading partner 1000=600"; got != want {
		t.Errorf("SAP's balances by trading partner of a balances run: got %s, want %s", got, want)
	}
}

// balanceTerms renders balances in key order.
func balanceTerms(m map[string]money.Minor) string {
	var out []string
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k].Plain(0))
	}
	return strings.Join(out, " ")
}
