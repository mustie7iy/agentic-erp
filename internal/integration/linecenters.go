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
// ACDOCA's controlling area is not read: a line's center is the one of its
// company code (CSKS-BUKRS for a cost center, CEPC_BUKRS for a profit
// center), a company code being taken to belong to one area, as the
// center master data already is (docs/migration.md).

// lineCenterIndex finds the centers a line names in the extract's master
// data: the controlling areas of each company code's cost centers and of
// the profit centers assigned to it, by company code/code, and each profit
// center's segment, by area/code.
type lineCenterIndex struct {
	cost, profit map[string][]string
	segment      map[string]string
}

func lineCentersOf(e *sap.Extract) lineCenterIndex {
	x := lineCenterIndex{cost: map[string][]string{}, profit: map[string][]string{}, segment: map[string]string{}}
	add := func(m map[string][]string, key, area string) {
		if !slices.Contains(m[key], area) {
			m[key] = append(m[key], area)
			slices.Sort(m[key])
		}
	}
	for _, r := range e.CSKS {
		add(x.cost, r.BUKRS+"/"+r.KOSTL, r.KOKRS)
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
func lineCenter(areas []string, kind, code, company, tables string) (key, why string) {
	switch len(areas) {
	case 0:
		return "", fmt.Sprintf("%s %s is not one of company %s's in the extract (%s), so the run does not migrate it", kind, code, company, tables)
	case 1:
		return areas[0] + "/" + code, ""
	}
	return "", fmt.Sprintf("%s %s of company %s is in controlling areas %s; a company code is taken to belong to one", kind, code, company,
		strings.Join(areas, ", "))
}

// centers are the keys of the centers a line names (empty: none), or why
// it cannot load: the refusals of the quality gate, which the
// reconciliation repeats for an extract read again without it.
func (x lineCenterIndex) centers(l sap.ACDOCA) (cost, profit string, why []string) {
	if l.RCNTR != "" {
		var w string
		if cost, w = lineCenter(x.cost[l.RBUKRS+"/"+l.RCNTR], "cost center", l.RCNTR, l.RBUKRS, "CSKS"); w != "" {
			why = append(why, w)
		}
	}
	if l.PRCTR != "" {
		var w string
		if profit, w = lineCenter(x.profit[l.RBUKRS+"/"+l.PRCTR], "profit center", l.PRCTR, l.RBUKRS, "CEPC_BUKRS"); w != "" {
			why = append(why, w)
		}
	}
	switch {
	case l.SEGMENT == "":
	case l.PRCTR == "":
		why = append(why, fmt.Sprintf("segment %s without a profit center; the platform takes a line's segment from its profit center", l.SEGMENT))
	case profit != "" && x.segment[profit] != l.SEGMENT:
		why = append(why, fmt.Sprintf("segment %s is not profit center %s's (%s)", l.SEGMENT, l.PRCTR, orNone(x.segment[profit])))
	}
	return cost, profit, why
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
