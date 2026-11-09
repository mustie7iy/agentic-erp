package integration

import (
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The ledger of SAP's journal lines (ACDOCA RLDNR; review:T56h4, ADR-0050
// As built). The Universal Journal holds a line once per ledger, and the
// other ledgers (local GAAP, extension ledgers holding delta lines) must
// never be added to the leading one, which the SAP specialist names 0L
// (PO 2026-11-09, to be confirmed in FINSC_LEDGER). The scope keeps the
// leading ledger's lines alone (sap.FilterScope); the export is to hold
// only those, so lines of another ledger in it are refused by the quality
// gate and the reconciliation as a safety net, naming the ledgers and
// their lines, rather than the run going on without them unnoticed.

// ledgerIssue is the error that refuses an extract holding lines of other
// ledgers than its leading ledger, and false when it holds none.
func ledgerIssue(e *sap.Extract) (Issue, bool) {
	if len(e.OtherLedgers) == 0 {
		return Issue{}, false
	}
	var others []string
	for _, l := range sortedKeys(e.OtherLedgers) {
		n := e.OtherLedgers[l]
		others = append(others, fmt.Sprintf("%q: %d %s", l, n, map[bool]string{true: "line", false: "lines"}[n == 1]))
	}
	return Issue{Severity: SevError, Table: "ACDOCA", Key: "RLDNR", Message: fmt.Sprintf(
		"the extract holds journal lines of other ledgers than the leading ledger %q (RLDNR %s): they are left out, never posted or summed, "+
			"and the run is refused, as the export is to hold the leading ledger's lines alone (WHERE RLDNR = '%s')",
		e.Ledger, strings.Join(others, ", "), e.Ledger)}, true
}
