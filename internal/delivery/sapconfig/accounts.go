package sapconfig

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// bankAccounts are a company's house bank accounts (T012K), by house bank
// (HBKID) in the extract's order: what T042I's account ID (HKTID) chooses
// among, per payment method and currency (SAP specialist, po:2026-11-09,
// A4 [H]; ADR-0038 As built, a house bank's account by payment method).
type bankAccounts map[string][]sap.T012K

// accountsOf are company's house bank accounts.
func (g *gen) accountsOf(company string) bankAccounts {
	out := bankAccounts{}
	for _, b := range g.e.T012K {
		if b.BUKRS == company {
			out[b.HBKID] = append(out[b.HBKID], b)
		}
	}
	return out
}

// accountID is a T012K row's account ID, or a T042I row's, as compared.
func accountID(id string) string { return strings.TrimSpace(id) }

// identified reports whether each of the house bank's accounts has an ID
// of its own (HKTID), so T042I can name one: false for a bank the company
// does not have, and for an extract without the IDs, whose bank keeps one
// account, its first, as before the draft read T042I.
func (a bankAccounts) identified(bank string) bool {
	rows := a[bank]
	seen := map[string]bool{}
	for _, b := range rows {
		id := accountID(b.HKTID)
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return len(rows) > 0
}

// several reports whether the house bank's accounts are each a house bank
// of the configuration: it has several, each with an ID of its own.
func (a bankAccounts) several(bank string) bool {
	return len(a[bank]) > 1 && a.identified(bank)
}

// code is the house bank code of account b: its house bank's (HBKID)
// where that has one account, else house bank and account ID (HBKID +
// HKTID), as the product owner decided on 2026-11-10 (the house bank code
// is per account). A bank whose several accounts the extract does not
// tell apart keeps its code, and its first account.
func (a bankAccounts) code(b sap.T012K) string {
	if a.several(b.HBKID) {
		return b.HBKID + accountID(b.HKTID)
	}
	return b.HBKID
}

// key is a T012K row's key in the fit-gap report: company code and house
// bank, with the account ID for a bank of several accounts, or for one
// whose accounts the extract does not tell apart the further account's
// place among them (n, from 1).
func (a bankAccounts) key(b sap.T012K, n int) string {
	key := b.BUKRS + "/" + b.HBKID
	switch {
	case a.several(b.HBKID):
		key += "/" + accountID(b.HKTID)
	case n > 1:
		key += fmt.Sprintf(" (account %d)", n)
	}
	return key
}

// named is the code of the account with id of the house bank, and whether
// the company has it.
func (a bankAccounts) named(bank, id string) (string, bool) {
	at := slices.IndexFunc(a[bank], func(b sap.T012K) bool { return accountID(b.HKTID) == accountID(id) })
	if at < 0 {
		return bank + accountID(id), false
	}
	return a.code(a[bank][at]), true
}

// pick is the code of a house bank account a ranking names; refused is
// set when it is no house bank of the company and the draft noted why
// validation refuses it.
type pick struct {
	code    string
	refused bool
}

// chosen are the accounts of house bank bank that pay by payment method
// method in currency, a T042A row's ("" for one without): the account
// T042I names for them, or, for a row without a currency, those T042I
// names for the method in any currency, each once (T042I keys one account
// per bank, method and currency). Where T042I names none, the
// bank's first account is ranked, as before the draft read T042I, noted
// for a bank of several accounts; an account T042I names that T012K does
// not give the company is ranked by the code it would have, and, unless
// that is another house bank's code (banks), noted as refused, which
// validation does. A bank the company does not have, or whose accounts
// the extract does not tell apart, is ranked as it is.
func (g *gen) chosen(o objRef, mp string, accounts bankAccounts, banks map[string]string, method, bank, currency string) []pick {
	if !accounts.identified(bank) {
		return []pick{{code: bank}}
	}
	var out []pick
	for _, i := range g.e.T042I {
		waers := strings.TrimSpace(i.WAERS)
		if i.ZBUKR != accounts[bank][0].BUKRS || i.HBKID != bank || i.ZLSCH != method || (currency != "" && waers != currency) {
			continue
		}
		code, ok := accounts.named(bank, i.HKTID)
		if slices.ContainsFunc(out, func(k pick) bool { return k.code == code }) {
			continue // named for another currency too: ranked once, for its own
		}
		_, other := banks[code] // an empty HKTID names the bank of one account itself; or two codes join alike
		refused := !ok && !other
		if refused {
			g.note(o, mp+".ranking", fmt.Sprintf("T042I names account %q of house bank %s for the method in %q, which T012K does not give the company: "+
				"ranked as %s, which validation refuses; add the account or correct T042I", accountID(i.HKTID), bank, waers, code))
		}
		out = append(out, pick{code: code, refused: refused})
	}
	if len(out) > 0 {
		return out
	}
	first := accounts.code(accounts[bank][0])
	if accounts.several(bank) {
		g.note(o, mp+".ranking", fmt.Sprintf("T042I names no account of house bank %s for the method %s, so the bank is known but not its account "+
			"(SAP specialist, po:2026-11-09, A4): its first account %s is ranked, as the draft did before it read T042I; check which account pays",
			bank, rowsFor(currency), first))
	}
	return []pick{{code: first}}
}
