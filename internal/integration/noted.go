package integration

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// SAP's noted items (ACDOCA BSTAT S; review:batch63, ADR-0037 As built). A
// noted item, such as a down payment request (F-47, special G/L F), is a
// statistical document: SAP posts nothing to the G/L for it and shows it
// among a vendor's open items only (the SAP specialist, po:2026-11-09, A14
// [H], A15 [M]). On the platform a down payment request is a payables
// document outside the ledger (ADR-0037), so a noted item is no journal
// entry and no open payable: the scope sets them aside (sap.FilterScope,
// Extract.Noted), nothing that reads the journal lines sees them, and the
// quality gate reports them as warnings rather than refusing the run,
// since nothing in SAP is wrong and nothing there could be corrected. The
// open ones are listed one by one: the requests still wanted are entered
// on the platform.

// noted reports the noted items the scope left out: per company code how
// many there are, and each one SAP has not cleared.
func (q *qualityCheck) noted() {
	lines := slices.Clone(q.e.Noted)
	slices.SortStableFunc(lines, func(a, b sap.ACDOCA) int {
		return cmp.Or(cmp.Compare(documentKey(a), documentKey(b)), cmp.Compare(a.DOCLN, b.DOCLN))
	})
	count, documents, open := map[string]int{}, map[string]map[string]bool{}, map[string]int{}
	for _, l := range lines {
		if documents[l.RBUKRS] == nil {
			documents[l.RBUKRS] = map[string]bool{}
		}
		count[l.RBUKRS]++
		documents[l.RBUKRS][documentKey(l)] = true
		if l.AUGBL == "" {
			open[l.RBUKRS]++
		}
	}
	for _, cc := range sortedKeys(count) {
		q.add(SevWarning, "ACDOCA", cc, "noted items (BSTAT S, such as down payment requests) of company %s are not migrated: lines %d, documents %d, "+
			"open in SAP (AUGBL blank; each listed) %d. SAP posts nothing to the G/L for a noted item, so it loads as no journal entry and counts as "+
			"no open item", cc, count[cc], len(documents[cc]), open[cc])
	}
	for _, l := range lines {
		if l.AUGBL == "" {
			q.add(SevWarning, "ACDOCA", documentKey(l)+"/"+l.DOCLN, "open noted item (BSTAT S)%s is not migrated; if it is still wanted, enter it on the "+
				"platform (a down payment request is a payables document, not a journal entry)", q.notedItem(l))
		}
	}
}

// notedItem names what a noted line is about, as far as the line says:
// its vendor by number and name, as the gate's other findings name one,
// its special G/L indicator, posting date and amount in company currency,
// and in its document currency where that is another: a request is in
// its document currency (po:2026-11-09, A14), which is what entering it
// again needs.
func (q *qualityCheck) notedItem(l sap.ACDOCA) string {
	var parts []string
	if l.LIFNR != "" {
		of := "of vendor " + l.LIFNR
		if name := q.vendor[l.LIFNR].NAME1; name != "" { // a vendor the extract does not hold has none
			of += fmt.Sprintf(" %q", name)
		}
		parts = append(parts, of)
	}
	if l.UMSKZ != "" {
		parts = append(parts, fmt.Sprintf("with special G/L indicator %q", l.UMSKZ))
	}
	cur := q.currency[l.RBUKRS]
	parts = append(parts, "posted on "+l.BUDAT, "over "+q.notedAmount(l.HSL, cur))
	if l.RWCUR != "" && l.RWCUR != cur {
		parts = append(parts, "("+q.notedAmount(l.WSL, l.RWCUR)+" in its document currency)")
	}
	return " " + strings.Join(parts, " ")
}

// notedAmount shows an amount of a noted line, as the extract holds it
// when it does not parse: it is never posted, so it is not refused.
func (q *qualityCheck) notedAmount(raw, cur string) string {
	amt, err := q.t.amount(raw, cur)
	if err != nil {
		return fmt.Sprintf("%q %s (as extracted)", raw, cur)
	}
	return amt.Plain(q.t.exponents[cur]) + " " + cur
}
