package sapconfig

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// paymentMethods are the company's payment methods (ADR-0038): each
// method T042E sets up for the company code, named by T042Z in the
// company's country and ranking, per currency, the house banks T042A
// ranks for it. What the configuration has no counterpart for (T042E's
// amount limits, T042I's bank subaccounts) is noted; a value SAP does not
// give (a method's name, its ranking) is left empty with a note, which
// validation refuses until a reviewer sets it.
func (g *gen) paymentMethods(p, country string, co *config.Company) []config.PaymentMethod {
	var out []config.PaymentMethod
	seen := map[string]bool{}
	for _, e := range g.e.T042E {
		if e.ZBUKR != co.Code || seen[e.ZLSCH] {
			continue
		}
		seen[e.ZLSCH] = true
		mp := p + ".payment_methods[" + e.ZLSCH + "]"
		g.amountLimits(mp, e, co.Currency)
		out = append(out, config.PaymentMethod{Code: e.ZLSCH, Description: g.methodName(mp, country, e.ZLSCH), Ranking: g.ranking(mp, co, e.ZLSCH)})
		g.subaccounts(mp, co, e.ZLSCH)
	}
	for _, a := range g.e.T042A {
		if a.ZBUKR == co.Code && !seen[a.ZLSCH] {
			seen[a.ZLSCH] = true
			g.note(p+".payment_methods", fmt.Sprintf("T042A ranks house banks for payment method %s, which T042E does not set up for the company: left out", a.ZLSCH))
		}
	}
	if len(out) == 0 {
		g.note(p+".payment_methods", "T042E sets up no payment method for the company: a payment run then needs a house bank, and the migration refuses "+
			"a supplier whose company data lists one (LFB1-ZWELS)")
	}
	return out
}

// methodName is the method's name in the company's country (T042Z, set
// up per country), or empty with a note when T042Z has none: validation
// refuses a method without a description.
func (g *gen) methodName(mp, country, code string) string {
	for _, z := range g.e.T042Z {
		if z.LAND1 == country && z.ZLSCH == code {
			if name := strings.TrimSpace(z.TEXT1); name != "" {
				return name
			}
		}
	}
	g.note(mp+".description", fmt.Sprintf("T042Z names no payment method %s for the company's country %s; describe it (left empty, which fails validation)", code, country))
	return ""
}

// amountLimits notes T042E's minimum and maximum amount of a payment by
// the method, in the company's currency: the configuration has none per
// method (ADR-0038 keeps amount limits out of scope), so a proposal would
// pay any amount by it.
func (g *gen) amountLimits(mp string, e sap.T042E, cur string) {
	if strings.Trim(e.VONBT+e.BISBT, "0., ") == "" {
		return
	}
	g.note(mp, fmt.Sprintf("SAP pays by the method only from %s to %s (T042E VONBT/BISBT); the configuration has no amount limits per method, "+
		"so payment proposals would pay any amount by it", g.shown(e.VONBT, cur), g.shown(e.BISBT, cur)))
}

// shown is an SAP amount as a note quotes it: converted (amount) where it
// can be, else as the export wrote it, saying so.
func (g *gen) shown(s, cur string) string {
	if strings.TrimSpace(s) == "" {
		return `""`
	}
	v, problem := g.amount(s, cur)
	if problem != "" {
		return fmt.Sprintf("%q as exported, which %s", strings.TrimSpace(s), problem)
	}
	return fmt.Sprintf("%q", v)
}

// ranked is a T042A row's place in its method's ranking: its order, or
// math.MaxInt for one that is not a number, which ranks last.
func ranked(mp string, a sap.T042A, note func(path, msg string)) int {
	n, err := strconv.Atoi(strings.TrimSpace(a.RANGF))
	if err != nil {
		note(mp+".ranking", fmt.Sprintf("T042A ranking order %q of house bank %s is not a number; ranked last", a.RANGF, a.HBKID))
		return math.MaxInt
	}
	return n
}

// ranking is the method's bank ranking per currency, from T042A in its
// ranking order (RANGF). A row with a currency ranks its bank for that
// currency; SAP uses a row without one for a currency the method has no
// row for, and a house bank here pays only in its own currency (ADR-0051
// §3), so such a bank is ranked for its account's currency unless the
// method ranks that currency already. A bank the company does not have,
// one ranked for another currency than its account's, or one ranked twice
// is kept and noted: validation refuses it until a reviewer corrects the
// ranking.
func (g *gen) ranking(mp string, co *config.Company, code string) []config.BankRanking {
	banks := map[string]string{} // house bank -> its currency
	for _, hb := range co.AllHouseBanks() {
		if hb.Name != "" {
			banks[hb.Name] = co.HouseBankCurrency(hb)
		}
	}
	type row struct {
		order int
		a     sap.T042A
	}
	var rows []row
	for _, a := range g.e.T042A {
		if a.ZBUKR == co.Code && a.ZLSCH == code {
			rows = append(rows, row{ranked(mp, a, g.note), a})
		}
	}
	if len(rows) == 0 {
		g.note(mp+".ranking", "T042A ranks no house bank for the method; rank the banks that pay with it (validation refuses a method without a ranking)")
		return nil
	}
	slices.SortStableFunc(rows, func(x, y row) int { return cmp.Compare(x.order, y.order) })
	byCurrency := map[string][]string{} // "": the rows without a currency
	for _, r := range rows {
		bank, currency := r.a.HBKID, strings.TrimSpace(r.a.WAERS) // HBKID as T012K's, which names the house banks
		g.rankedBank(mp, bank, currency, banks)
		if slices.Contains(byCurrency[currency], bank) {
			g.note(mp+".ranking", fmt.Sprintf("T042A ranks house bank %s twice %s; rank it once (validation refuses it)", bank, rowsFor(currency)))
		}
		byCurrency[currency] = append(byCurrency[currency], bank)
	}
	anyCurrency := byCurrency[""]
	delete(byCurrency, "")
	g.withoutCurrency(mp, co.Currency, anyCurrency, banks, byCurrency)
	out := make([]config.BankRanking, 0, len(byCurrency))
	for _, currency := range sortedKeys(byCurrency) {
		out = append(out, config.BankRanking{Currency: currency, HouseBanks: byCurrency[currency]})
	}
	return out
}

// rowsFor names the T042A rows of a currency, or those without one.
func rowsFor(currency string) string {
	if currency == "" {
		return "without a currency"
	}
	return "for " + currency
}

// rankedBank notes a T042A row validation will refuse: a bank the company
// does not have, or one ranked for another currency than its account's.
func (g *gen) rankedBank(mp, bank, currency string, banks map[string]string) {
	own, ok := banks[bank]
	switch {
	case !ok:
		g.note(mp+".ranking", fmt.Sprintf("T042A ranks house bank %s, which T012K does not give the company: drop it or add the bank (validation refuses it)", bank))
	case currency != "" && currency != own:
		g.note(mp+".ranking["+currency+"]", fmt.Sprintf("T042A ranks house bank %s, an account in %s, for payments in %s; a house bank pays only in its own currency "+
			"(ADR-0051 §3): rank another bank (validation refuses it)", bank, own, currency))
	}
}

// withoutCurrency ranks the banks of T042A rows without a currency, each
// for its account's currency (the company's for a bank the company does
// not have, which validation refuses), unless the method ranks that
// currency with rows of its own, which SAP would take instead.
func (g *gen) withoutCurrency(mp, companyCurrency string, unranked []string, banks map[string]string, byCurrency map[string][]string) {
	if len(unranked) == 0 {
		return
	}
	explicit := map[string]bool{}
	for currency := range byCurrency {
		explicit[currency] = true
	}
	for _, bank := range unranked {
		currency := cmp.Or(banks[bank], companyCurrency)
		if explicit[currency] {
			g.note(mp+".ranking["+currency+"]", fmt.Sprintf("T042A ranks house bank %s without a currency, which SAP uses only where the method ranks "+
				"no bank for the currency; it ranks banks for %s, so %s is not ranked", bank, currency, bank))
			continue
		}
		byCurrency[currency] = append(byCurrency[currency], bank)
	}
	g.note(mp+".ranking", "T042A ranks house banks without a currency, which SAP pays any currency through; a house bank pays only in its own "+
		"currency (ADR-0051 §3), so each is ranked for its account's")
}

// subaccounts notes T042I's bank subaccounts that are not the house
// bank's ledger account: SAP posts a payment by the method to the
// subaccount, the platform credits the bank's one clearing account for
// every method (ADR-0038).
func (g *gen) subaccounts(mp string, co *config.Company, code string) {
	for _, i := range g.e.T042I {
		if i.ZBUKR != co.Code || i.ZLSCH != code {
			continue
		}
		at := slices.IndexFunc(co.AllHouseBanks(), func(hb config.HouseBank) bool { return hb.Name != "" && hb.Name == i.HBKID })
		if at < 0 {
			continue // a bank the company does not have pays nothing
		}
		account, sub := co.HouseBankAccount(co.AllHouseBanks()[at]), sap.Alpha(strings.TrimSpace(i.UKONT))
		if sub != "" && sub != account {
			g.note(mp, fmt.Sprintf("SAP posts payments by the method from house bank %s in %q to its bank subaccount %s (T042I UKONT); the platform "+
				"credits the bank's one clearing account %s for every method: check it is the account SAP's outgoing payments clear", i.HBKID,
				strings.TrimSpace(i.WAERS), sub, account))
		}
	}
}
