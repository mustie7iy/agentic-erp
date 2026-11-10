package integration

import (
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// recAccount is the SAP side of one vendor reconciliation account at the
// key date: its balance, the open vendor items, and what else of the
// balance the extract tells apart (po:2026-11-09, A15).
type recAccount struct {
	balance, open              []money.Minor
	noVendor, special, cleared []money.Minor
	otherAccount               int // open items of vendors whose LFB1-AKONT is another account
}

// reconciliationAccounts checks, per vendor reconciliation account (SKB1
// MITKZ K that T074 names no special G/L account), that the
// open vendor items add up to the account's balance at the key date
// (ADR-0060 §9; the SAP specialist, po:2026-11-09, A15 [M]). The key date
// is the company's latest posting date in the extract; the balance is
// the account's lines (noted items, BSTAT S, have no G/L effect and are
// not among the journal lines: sap.FilterScope sets them aside); an open
// vendor item is a line with a vendor (LIFNR) and no special G/L
// indicator (UMSKZ), open at the key date (openAt). A difference is a warning, never a refusal: its message
// names the parts the extract tells apart, and the cases SAP knows that
// make one.
func (q *qualityCheck) reconciliationAccounts() {
	keyDate, accounts, akont := map[string]string{}, map[string]*recAccount{}, map[string]string{}
	for _, v := range q.e.LFB1 {
		akont[v.BUKRS+"/"+v.LIFNR] = sap.Alpha(v.AKONT)
	}
	for _, l := range q.e.ACDOCA {
		keyDate[l.RBUKRS] = max(keyDate[l.RBUKRS], l.BUDAT)
	}
	for _, l := range q.e.ACDOCA {
		key := l.RBUKRS + "/" + sap.Alpha(l.RACCT)
		amt, err := q.t.amount(l.HSL, q.currency[l.RBUKRS])
		if q.account[l.RBUKRS+"/"+l.RACCT].MITKZ != "K" || q.specialAccounts[key] || err != nil { // a bad amount is journalLine's to refuse
			continue
		}
		a := accounts[key]
		if a == nil {
			a = &recAccount{}
			accounts[key] = a
		}
		own, ok := akont[l.RBUKRS+"/"+l.LIFNR]
		a.add(l, amt, keyDate[l.RBUKRS], ok && own != sap.Alpha(l.RACCT))
	}
	for _, key := range sortedKeys(accounts) {
		company, _, _ := strings.Cut(key, "/")
		q.reconciliationAccount(key, keyDate[company], accounts[key])
	}
}

// add counts a line of the account.
func (a *recAccount) add(l sap.ACDOCA, amt money.Minor, keyDate string, otherAccount bool) {
	a.balance = append(a.balance, amt)
	switch {
	case l.LIFNR == "":
		a.noVendor = append(a.noVendor, amt)
	case l.UMSKZ != "":
		a.special = append(a.special, amt)
	case !openAt(l, keyDate):
		a.cleared = append(a.cleared, amt)
	default:
		a.open = append(a.open, amt)
		if otherAccount {
			a.otherAccount++
		}
	}
}

// openAt reports whether a line was open at the key date (YYYYMMDD): not
// cleared, or cleared after it by its clearing date (AUGDT, A3). A line
// cleared without a clearing date in the extract is taken as cleared.
func openAt(l sap.ACDOCA, keyDate string) bool {
	if l.AUGBL == "" {
		return true
	}
	cleared := sapDay(l.AUGDT, "")
	return cleared != "" && cleared > keyDate
}

// reconciliationAccount warns of an account whose open vendor items do
// not add up to its balance.
func (q *qualityCheck) reconciliationAccount(key, keyDate string, a *recAccount) {
	company, account, _ := strings.Cut(key, "/")
	cur := q.currency[company]
	sums := make([]money.Minor, 5)
	for i, terms := range [][]money.Minor{a.balance, a.open, a.noVendor, a.special, a.cleared} {
		var err error
		if sums[i], err = money.Sum(terms...); err != nil {
			return // journal refuses a company whose lines add up to more than an amount holds
		}
	}
	if sums[0] == sums[1] {
		return
	}
	shown := func(v money.Minor) string { return v.Plain(q.t.exponents[cur]) + " " + cur }
	parts := []string{fmt.Sprintf("lines without a vendor (LIFNR blank: a balance without vendor items, a period 000 carry-forward, a valuation posted to the account) %s", shown(sums[2])),
		fmt.Sprintf("special G/L lines (UMSKZ, which belong on an alternative reconciliation account, T074) %s", shown(sums[3])),
		fmt.Sprintf("vendor items cleared by the key date that do not net to zero in the extract (a clearing in another year or on another account) %s", shown(sums[4]))}
	msg := fmt.Sprintf("the open vendor items of reconciliation account %s at the key date %s add up to %s, its balance to %s; of the difference: %s",
		account, keyDate, shown(sums[1]), shown(sums[0]), strings.Join(parts, "; "))
	if a.otherAccount > 0 {
		msg += fmt.Sprintf("; %d open items belong to vendors whose reconciliation account (LFB1-AKONT) is now another", a.otherAccount)
	}
	q.add(SevWarning, "ACDOCA", key, "%s. It is reported, not refused (SAP specialist, to verify on the pilot system)", msg)
}
