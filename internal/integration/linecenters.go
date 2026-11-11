package integration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The centers on migrated journal lines (ADR-0050 §10, T56h4): a line's
// cost center (ACDOCA RCNTR), profit center (PRCTR) and segment (SEGMENT)
// load onto its journal line wherever the run posts SAP's document line by
// line (every document in documents mode; in balances mode the documents
// posted as entries of their own, ownEntry), where the
// ledger's derivation and validation check them as they check any
// posting's (§5): a profit center that is not the cost center's on the
// posting date, a center not valid or locked then, a cost center on an
// account that takes none. The quality gate refuses beforehand what the
// load cannot name: a center the run does not migrate, and a segment that
// is not the line's profit center's. A balances-mode run's opening entry
// carries balances, not SAP's lines, so none (§10: "an
// opening-balance-only migration ... needs none").
//
// A line's centers are those of its controlling area (ACDOCA KOKRS), the
// cost center one of its company code's (CSKS-BUKRS) and the profit center
// one assigned to it (CEPC_BUKRS). SAP leaves KOKRS blank in a company code
// without CO active (the SAP specialist, po:2026-11-09, A9(d) [H]), so a
// line naming a center without one is refused. A line keeps the profit
// center SAP derived from its cost center on its posting date (A9(b) [M]),
// which may differ from today's after a change of the cost center: the
// gate, and the reconciliation, which reads the extract again, compare it
// with the cost center's slice valid on the posting date, as the ledger
// does when the run posts it. SAP derives a line's segment from its profit
// center unless a substitution (the BAdI FAGL_DERIVE_SEGMENT) or a manual
// entry set it (A9(c) [M]); the platform gives a profit center one segment
// without validity and every line its profit center's (ADR-0050 §3, §5,
// held by a foreign key), so such a line is refused, naming the case, not
// loaded with a segment the ledger cannot hold. A profit center whose
// segment changed after posting has slices naming two segments, which the
// gate already refuses as a reorganisation. A cost center without a profit center then
// gave SAP's line the dummy profit center, which the ledger takes as it
// takes a profit center given beside a cost center without one.

// lineCenterIndex finds the centers a line names in the extract's master
// data: the controlling areas of each company code's cost centers and of
// the profit centers assigned to it, by company code/code, each cost
// center's slices, by area/code, and each profit center's segment, by
// area/code.
type lineCenterIndex struct {
	cost, profit map[string][]string
	slices       map[string][]sap.CSKS
	segment      map[string]string
}

func lineCentersOf(e *sap.Extract) lineCenterIndex {
	x := lineCenterIndex{cost: map[string][]string{}, profit: map[string][]string{}, slices: map[string][]sap.CSKS{}, segment: map[string]string{}}
	add := func(m map[string][]string, key, area string) {
		if !slices.Contains(m[key], area) {
			m[key] = append(m[key], area)
		}
	}
	for _, r := range e.CSKS {
		add(x.cost, r.BUKRS+"/"+r.KOSTL, r.KOKRS)
		x.slices[r.KOKRS+"/"+r.KOSTL] = append(x.slices[r.KOKRS+"/"+r.KOSTL], r)
	}
	for _, r := range e.CEPC_BUKRS {
		add(x.profit, r.BUKRS+"/"+r.PRCTR, r.KOKRS)
	}
	for _, pc := range profitCenters(e) {
		x.segment[pc.key()] = pc.slices[len(pc.slices)-1].SEGMENT
	}
	return x
}

// lineCenter is a center a line names, as the master data's key
// (KOKRS/code), or why it names none the run migrates.
func lineCenter(areas []string, area, kind, code, company, tables string) (key, why string) {
	if !slices.Contains(areas, area) {
		return "", fmt.Sprintf("%s %s is not one of company %s's of controlling area %s in the extract (%s), so the run does not migrate it",
			kind, code, company, area, tables)
	}
	return area + "/" + code, ""
}

// centers are the keys of the centers a line names (empty: none), or why
// it cannot load: the refusals of the quality gate, which the
// reconciliation repeats for an extract read again without it.
func (x lineCenterIndex) centers(l sap.ACDOCA) (cost, profit string, why []string) {
	if (l.RCNTR != "" || l.PRCTR != "") && l.KOKRS == "" {
		return "", "", []string{fmt.Sprintf("a line naming a center has no controlling area (KOKRS blank: CO is not active in company code %s)", l.RBUKRS)}
	}
	if l.RCNTR != "" {
		var w string
		if cost, w = lineCenter(x.cost[l.RBUKRS+"/"+l.RCNTR], l.KOKRS, "cost center", l.RCNTR, l.RBUKRS, "CSKS"); w != "" {
			why = append(why, w)
		}
	}
	if l.PRCTR != "" {
		var w string
		if profit, w = lineCenter(x.profit[l.RBUKRS+"/"+l.PRCTR], l.KOKRS, "profit center", l.PRCTR, l.RBUKRS, "CEPC_BUKRS"); w != "" {
			why = append(why, w)
		}
	}
	if cost != "" {
		why = append(why, x.postedProfitCenter(l, cost)...)
	}
	switch {
	case l.SEGMENT == "":
	case l.PRCTR == "":
		why = append(why, fmt.Sprintf("segment %s without a profit center; the platform takes a line's segment from its profit center", l.SEGMENT))
	case profit != "" && x.segment[profit] != l.SEGMENT:
		why = append(why, fmt.Sprintf("segment %s is not profit center %s's (%s): SAP set the line's segment otherwise than from the profit center "+
			"(the BAdI FAGL_DERIVE_SEGMENT or a manual entry), which the platform does not hold, a profit center having one segment",
			l.SEGMENT, l.PRCTR, orNone(x.segment[profit])))
	}
	return cost, profit, why
}

// postedProfitCenter checks a line's cost center against its slice valid
// on the posting date (BUDAT): there must be one, as the ledger refuses a
// cost center not valid then, and a profit center the line names must be
// that slice's, unless the slice has none (SAP's dummy profit center).
func (x lineCenterIndex) postedProfitCenter(l sap.ACDOCA, cost string) []string {
	i := slices.IndexFunc(x.slices[cost], func(r sap.CSKS) bool { return r.DATAB <= l.BUDAT && l.BUDAT <= r.DATBI })
	if i < 0 {
		return []string{fmt.Sprintf("cost center %s is not valid on the posting date %s (no CSKS slice of controlling area %s covers it)", l.RCNTR, l.BUDAT, l.KOKRS)}
	}
	if r := x.slices[cost][i]; l.PRCTR != "" && r.PRCTR != "" && r.PRCTR != l.PRCTR {
		return []string{fmt.Sprintf("profit center %s is not cost center %s's on the posting date %s (%s, its slice %s to %s)",
			l.PRCTR, l.RCNTR, l.BUDAT, r.PRCTR, r.DATAB, r.DATBI)}
	}
	return nil
}

// lineCenters refuses a journal line the run posts (every one in
// documents mode, those of the documents posted on their own in balances
// mode) naming a center the run does not migrate or a segment that is not
// its profit center's.
func (q *qualityCheck) lineCenters() {
	x, own := lineCentersOf(q.e), downPaymentDocs(q.e)
	for _, l := range q.e.ACDOCA {
		if key := documentKey(l); q.t.balances && own[key] == nil && q.foreign[key] == "" {
			continue // in the opening entry, which carries no centers
		}
		_, _, why := x.centers(l)
		for _, w := range why {
			q.add(SevError, "ACDOCA", documentKey(l)+"/"+l.DOCLN, "%s", w)
		}
	}
}

// assignCenters sets a posted line's centers from the master data runs
// loaded, or says which one was not loaded: the load refused it, so the
// document is refused too rather than posted without it.
func (l *loader) assignCenters(line sap.ACDOCA, pl *ledger.PostingLine) string {
	cost, profit, _ := l.lines.centers(line) // the quality gate refused a center the run does not migrate
	for _, c := range []struct {
		ref, what string
		into      **uuid.UUID
	}{{"CSKS/" + cost, "cost center " + line.RCNTR, &pl.CostCenterID}, {"CEPC/" + profit, "profit center " + line.PRCTR, &pl.ProfitCenterID},
		{"FAGL_SEGM/" + line.SEGMENT, "segment " + line.SEGMENT, &pl.SegmentID}} {
		if strings.HasSuffix(c.ref, "/") {
			continue
		}
		id, ok := l.refs[c.ref]
		if !ok {
			return c.what + " was not loaded"
		}
		*c.into = &id
	}
	return ""
}
