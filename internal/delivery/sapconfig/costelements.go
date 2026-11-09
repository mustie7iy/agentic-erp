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

// areasOf are the controlling areas of each company code: the one TKA02
// assigns it to, as a company code belongs to exactly one (the SAP
// specialist, po:2026-11-09, A8), else, for an extract without TKA02 (an
// OData one), the areas of its cost centers (CSKS) and of the profit
// centers assigned to it (CEPC_BUKRS).
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
// from SAP's cost element categories (CSKB-KATYP of the account's latest
// slice in the company's controlling areas; T56h2): 01 primary_cost on an
// expense account, 11 or 12 revenue on a revenue account, and none on an
// account that is not a cost element in SAP (no CSKB row), as S/4HANA
// keeps non-operating accounts (from memory, pending the SAP specialist).
// A category the account's type cannot take or the platform does not
// hold, or areas that disagree, leaves the type's default with a note. A company whose areas the extract does not tell,
// or whose areas have no cost elements, keeps the defaults.
func (g *gen) costElements(p, company string, accounts []config.Account) {
	areas := g.areas[company]
	byAccount := map[string]map[string]sap.CSKB{} // account, then area: the latest slice
	for _, r := range g.e.CSKB {
		if !slices.Contains(areas, r.KOKRS) {
			continue
		}
		num := sap.Alpha(r.KSTAR)
		if byAccount[num] == nil {
			byAccount[num] = map[string]sap.CSKB{}
		}
		if cur, ok := byAccount[num][r.KOKRS]; !ok || r.DATBI > cur.DATBI {
			byAccount[num][r.KOKRS] = r
		}
	}
	if len(byAccount) == 0 {
		if len(g.e.CSKB) > 0 {
			g.note(p+".chart_of_accounts", "the extract has no cost elements (CSKB) of the company's controlling areas ("+
				cmp.Or(strings.Join(areas, ", "), "none known: the company has no centers, and the extract no TKA02 row of it")+"); its accounts take their type's cost element")
		}
		return
	}
	for i := range accounts {
		a := &accounts[i]
		if a.Type == config.Revenue || a.Type == config.Expense {
			a.CostElement = g.costElement(p+".chart_of_accounts["+a.Number+"].cost_element", *a, byAccount[a.Number])
		}
	}
}

// costElement is a profit and loss account's cost element from its CSKB
// rows by controlling area.
func (g *gen) costElement(path string, a config.Account, rows map[string]sap.CSKB) config.CostElement {
	if len(rows) == 0 {
		g.note(path, "not a cost element in SAP (no CSKB row): none, so its lines take no cost object")
		return config.CostElementNone
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
