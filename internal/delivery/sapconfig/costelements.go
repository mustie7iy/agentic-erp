package sapconfig

import (
	"cmp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Cost element categories (CSKB-KATYP, ADR-0050 "What SAP does"): 01
// primary costs, 11 revenues and 12 sales deductions. The others are
// secondary or statistical elements, which the platform does not hold
// (secondary is refused until T60, ADR-0050 §4).
const (
	katypPrimaryCost     = "01"
	katypRevenue         = "11"
	katypSalesDeductions = "12"
)

// G/L account types (SKA1-GLACCOUNT_TYPE, A12(c)): N non-operating, P
// primary costs or revenue, S secondary costs (X balance sheet).
const (
	glTypeNonOperating = "N"
	glTypePrimary      = "P"
	glTypeSecondary    = "S"
)

// areasOf are the controlling areas of each company code: the one TKA02
// assigns it to, as a company code belongs to exactly one (the SAP
// specialist, po:2026-11-09, A8), else, for an extract without TKA02 (an
// OData one), the areas of its cost centers (CSKS) and of the profit
// centers assigned to it (CEPC_BUKRS). TKA02 wins over the areas the
// centers give, which SAP cannot make disagree with it.
func areasOf(e *sap.Extract) map[string][]string {
	out := map[string][]string{}
	add := func(company, area string) {
		if !slices.Contains(out[company], area) {
			out[company] = append(out[company], area)
		}
	}
	for _, r := range e.CSKS {
		add(r.BUKRS, r.KOKRS)
	}
	for _, r := range e.CEPC_BUKRS {
		add(r.BUKRS, r.KOKRS)
	}
	for _, areas := range out {
		slices.Sort(areas)
	}
	for _, r := range e.TKA02 {
		out[r.BUKRS] = []string{r.KOKRS}
	}
	return out
}

// costElements sets the company's profit and loss accounts' cost_element
// from SAP's cost element categories (CSKB-KATYP of the account's slice
// valid at the cutover date in the company's controlling areas, A12(d);
// T56h2): 01 primary_cost on an expense account, 11 or 12 revenue on a
// revenue account, and none on an account that is not a cost element in
// SAP (no CSKB row), as S/4HANA keeps non-operating accounts (A12(c)),
// which SKA1-GLACCOUNT_TYPE cross-checks. A category the account's type
// cannot take or the platform does not hold, or areas that disagree,
// leaves the type's default with a note. A company whose areas the
// extract does not tell, or whose areas have no cost elements, keeps the
// defaults.
func (g *gen) costElements(p, company string, accounts []config.Account) {
	areas := g.areas[company]
	byAccount := map[string]map[string][]sap.CSKB{} // account, then area: its slices
	for _, r := range g.e.CSKB {
		if !slices.Contains(areas, r.KOKRS) {
			continue
		}
		num := sap.Alpha(r.KSTAR)
		if byAccount[num] == nil {
			byAccount[num] = map[string][]sap.CSKB{}
		}
		byAccount[num][r.KOKRS] = append(byAccount[num][r.KOKRS], r)
	}
	if len(byAccount) == 0 {
		if len(g.e.CSKB) > 0 {
			g.note(p+".chart_of_accounts", "the extract has no cost elements (CSKB) of the company's controlling areas ("+
				cmp.Or(strings.Join(areas, ", "), "none known: the company has no centers, and the extract no TKA02 row of it")+"); its accounts take their type's cost element")
		}
		return
	}
	if g.opts.Cutover == "" {
		g.note(p+".chart_of_accounts", "no cutover date given (-cutover): each account's cost element is its latest CSKB slice's, "+
			"not the one valid at the cutover")
	}
	types := map[string]string{}
	for _, a := range g.e.SKB1 {
		if a.BUKRS == company {
			types[sap.Alpha(a.SAKNR)] = a.GLACCOUNT_TYPE
		}
	}
	for i := range accounts {
		a := &accounts[i]
		if a.Type == config.Revenue || a.Type == config.Expense {
			path := p + ".chart_of_accounts[" + a.Number + "].cost_element"
			a.CostElement = g.costElement(path, *a, g.cutoverSlices(path, byAccount[a.Number]), types[a.Number])
		}
	}
}

// cutoverSlices are an account's CSKB slices to read, one per controlling
// area: the one valid at the cutover date, else (none covers it, or no
// date was given) the latest, with a note when the date was given: an
// expired cost element means CO postings to it were stopped on purpose.
func (g *gen) cutoverSlices(path string, rows map[string][]sap.CSKB) map[string]sap.CSKB {
	out := map[string]sap.CSKB{}
	for _, area := range sortedKeys(rows) {
		list := rows[area]
		latest := list[0]
		for _, r := range list {
			if g.opts.Cutover != "" && r.DATAB <= g.opts.Cutover && g.opts.Cutover <= r.DATBI {
				out[area] = r
				break
			}
			if r.DATBI > latest.DATBI {
				latest = r
			}
		}
		if _, ok := out[area]; !ok {
			out[area] = latest
			if g.opts.Cutover != "" {
				g.note(path, "no CSKB slice of controlling area "+area+" is valid at the cutover date "+g.opts.Cutover+"; its latest, to "+
					latest.DATBI+", is taken: an expired cost element means CO postings to it were stopped on purpose")
			}
		}
	}
	return out
}

// costElement is a profit and loss account's cost element from its CSKB
// rows by controlling area, cross-checked by its G/L account type
// (SKA1-GLACCOUNT_TYPE) when it has none.
func (g *gen) costElement(path string, a config.Account, rows map[string]sap.CSKB, glType string) config.CostElement {
	if len(rows) == 0 {
		return g.noCostElement(path, glType)
	}
	var katyp []string
	for _, area := range sortedKeys(rows) {
		if k := rows[area].KATYP; !slices.Contains(katyp, k) {
			katyp = append(katyp, k)
		}
	}
	if len(katyp) > 1 {
		g.note(path, "the controlling areas give categories "+strings.Join(katyp, ", ")+" (CSKB-KATYP); left at its type's default")
		return ""
	}
	switch {
	case katyp[0] == katypPrimaryCost && a.Type == config.Expense:
		return config.CostElementPrimaryCost
	case (katyp[0] == katypRevenue || katyp[0] == katypSalesDeductions) && a.Type == config.Revenue:
		return config.CostElementRevenue
	case katyp[0] == katypPrimaryCost || katyp[0] == katypRevenue || katyp[0] == katypSalesDeductions:
		g.note(path, "category "+katyp[0]+" (CSKB-KATYP) does not fit the account's type, "+string(a.Type)+"; left at its type's default")
	default:
		g.note(path, "category "+katyp[0]+" (CSKB-KATYP) is a secondary or statistical cost element, which the platform does not hold; left at its type's default")
	}
	return ""
}

// noCostElement is the cost element of an account without a CSKB row:
// none, unless its G/L account type (SKA1-GLACCOUNT_TYPE) says it is one,
// which leaves its type's default with a note.
func (g *gen) noCostElement(path, glType string) config.CostElement {
	switch glType {
	case "", glTypeNonOperating:
		g.note(path, "not a cost element in SAP (no CSKB row): none, so its lines take no cost object")
		return config.CostElementNone
	case glTypePrimary, glTypeSecondary:
		g.note(path, "no CSKB row, but its G/L account type (SKA1-GLACCOUNT_TYPE) "+glType+" says it is a cost element; left at its type's default")
	default:
		g.note(path, "no CSKB row, and its G/L account type (SKA1-GLACCOUNT_TYPE) "+glType+" is not a profit and loss one; left at its type's default")
	}
	return ""
}
