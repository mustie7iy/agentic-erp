package sapconfig

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// SAP's default account assignments (OKB9; ADR-0050 §10, T56h3) as the SAP
// specialist described them (PO, 2026-11-09): TKA3A gives a company code's
// cost element a cost center, internal order or profit center, unless its
// detail indicator REGEL says SAP determines the object per valuation area
// (1) or business area (2) from TKA3C, or per profit center (3) from TKA3P.
// The platform keeps one default cost center per company and account
// (default_assignments), so only REGEL blank with a cost center becomes
// one; every other row is noted for the reviewers. Even that one is not in
// the draft itself: the draft is the tenant's first configuration, which
// is provisioned before the migration run loads the cost centers, and the
// ledger activates a default only for a cost center it holds (ADR-0050 §4,
// ledger.Setup.Check). Its note gives the entry to propose in a
// configuration change once the run has loaded the cost center.

// regelMeaning names what a detail indicator makes SAP read.
var regelMeaning = map[string]string{
	"1": "valuation area mandatory: SAP takes the object per valuation area from TKA3C",
	"2": "business area mandatory: SAP takes the object per business area from TKA3C",
	"3": "profit center mandatory: SAP takes the object per profit center from TKA3P",
}

// defaultAssignments notes the company's TKA3A rows in account order: the
// entry to propose after the migration for a row that is the general
// assignment (REGEL blank) of a cost center the migration loads in the
// company, valid without end and open for actual costs, on an account of
// the company's chart that needs a cost center (primary_cost); why not for
// every other row, never taking TKA3A's own cost center as a fallback (the
// specialist: no fallback when REGEL is set). Called after costElements,
// whose cost elements it reads.
func (g *gen) defaultAssignments(p string, co *config.Company) {
	rows := slices.DeleteFunc(slices.Clone(g.e.TKA3A), func(r sap.TKA3A) bool { return r.BUKRS != co.Code })
	slices.SortFunc(rows, func(a, b sap.TKA3A) int { return strings.Compare(sap.Alpha(a.KSTAR), sap.Alpha(b.KSTAR)) })
	for _, r := range rows {
		account := sap.Alpha(r.KSTAR)
		path := p + ".default_assignments[" + account + "]"
		if why := g.notProposed(r, co); why != "" {
			g.note(path, "TKA3A "+r.KOKRS+"/"+account+": "+why+"; not proposed")
			continue
		}
		msg := fmt.Sprintf("propose after the migration: {account: %q, cost_center: %q}, the default cost center from TKA3A (OKB9, REGEL blank); "+
			"the ledger activates a default only for a cost center it holds, so it is left out of the first configuration until the run has loaded %s",
			account, r.KOSTL, r.KOSTL)
		if extra := objects("", r.AUFNR, r.PRCTR); extra != "" {
			msg += "; TKA3A's " + extra + " beside it is not proposed, as a default holds a cost center alone"
		}
		g.note(path, msg)
	}
}

// notProposed is why a TKA3A row is not proposed as a default assignment of
// the company, or "".
func (g *gen) notProposed(r sap.TKA3A, co *config.Company) string {
	if why := indicators(r, g.details); why != "" {
		return why
	}
	account := sap.Alpha(r.KSTAR)
	switch {
	case r.KOSTL == "":
		return cmp.Or(objects("", r.AUFNR, r.PRCTR), "no cost center, order or profit center") +
			" without a cost center: the platform's default assignments hold cost centers only"
	case co.Account(account) == nil:
		return "account " + account + " is not in the company's chart of accounts"
	case co.Account(account).CostElementOrDefault() != config.CostElementPrimaryCost:
		return fmt.Sprintf("account %s has cost element %s; a default cost center is only for accounts that need one (primary_cost)",
			account, co.Account(account).CostElementOrDefault())
	}
	return g.costCenterProblem(co.Code, r.KOKRS, r.KOSTL)
}

// indicators is why a row's detail indicator (REGEL) or its substitution
// (BSSUBST) keeps it from being a default, naming each that is set, or "".
func indicators(r sap.TKA3A, details func(sap.TKA3A) string) string {
	var why []string
	switch {
	case regelMeaning[r.REGEL] != "":
		w := "REGEL " + r.REGEL + ", " + regelMeaning[r.REGEL] + " (" + details(r) + "); the platform keeps one cost center per company and account"
		if own := objects(r.KOSTL, r.AUFNR, r.PRCTR); own != "" {
			w += ", and TKA3A's own " + own + " is not taken as a fallback"
		}
		why = append(why, w)
	case r.REGEL != "":
		why = append(why, "REGEL "+r.REGEL+" is not a detail indicator the specialist described (blank, 1, 2 or 3)")
	}
	if r.BSSUBST != "" {
		why = append(why, "the profitability segment is found by substitution (BSSUBST "+r.BSSUBST+"), which the platform does not have")
	}
	return strings.Join(why, "; ")
}

// costCenterProblem is why the cost center cannot be a default of the
// company, or "": the migration run loads a cost center in the company
// when every CSKS slice of it is in the company code (one in two company
// codes is refused by its quality gate), and the ledger takes a default
// only valid without end and open for actual costs from today (ADR-0050
// §4), so its latest slice must run to 99991231 without BKZKP.
func (g *gen) costCenterProblem(company, area, code string) string {
	var latest *sap.CSKS
	for i, r := range g.e.CSKS {
		if r.KOKRS != area || r.KOSTL != code {
			continue
		}
		if r.BUKRS != company {
			latest = nil
			break
		}
		if latest == nil || r.DATBI > latest.DATBI {
			latest = &g.e.CSKS[i]
		}
	}
	switch {
	case latest == nil:
		return "cost center " + area + "/" + code + " is not one of the company's cost centers in the extract (CSKS), so the migration does not load it"
	case latest.DATBI != "99991231":
		return "cost center " + area + "/" + code + " ends on " + latest.DATBI + " (its latest CSKS slice), so the ledger takes no default on it"
	case latest.BKZKP == "X":
		return "cost center " + area + "/" + code + " is locked for actual costs (BKZKP X in its latest CSKS slice), so the ledger takes no default on it"
	}
	return ""
}

// details are the TKA3C or TKA3P rows the detail indicator of r reads,
// each with the object it names, or says there are none.
func (g *gen) details(r sap.TKA3A) string {
	var out []string
	if r.REGEL == "3" {
		for _, d := range g.e.TKA3P {
			if d.BUKRS == r.BUKRS && d.KSTAR == r.KSTAR {
				out = append(out, "TKA3P profit center "+d.PRCTR+": "+cmp.Or(objects(d.KOSTL, d.AUFNR, ""), "no object"))
			}
		}
		return cmp.Or(strings.Join(out, "; "), "the extract has no TKA3P rows for it")
	}
	for _, d := range g.e.TKA3C {
		if d.BUKRS == r.BUKRS && d.KSTAR == r.KSTAR {
			out = append(out, "TKA3C valuation area "+blank(d.BWKEY)+", business area "+blank(d.GSBER)+": "+
				cmp.Or(objects(d.KOSTL, d.AUFNR, d.PRCTR), "no object"))
		}
	}
	return cmp.Or(strings.Join(out, "; "), "the extract has no TKA3C rows for it")
}

// objects names the cost center, order and profit center given, or "".
func objects(kostl, aufnr, prctr string) string {
	var out []string
	for _, o := range [][2]string{{"cost center ", kostl}, {"order ", aufnr}, {"profit center ", prctr}} {
		if o[1] != "" {
			out = append(out, o[0]+o[1])
		}
	}
	return strings.Join(out, ", ")
}

// blank shows an empty key as (blank), as TKA3C keeps a blank area.
func blank(s string) string { return cmp.Or(s, "(blank)") }
