package integration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// The ledger of SAP's journal lines (ACDOCA RLDNR; review:T56h4, ADR-0050
// As built). The Universal Journal holds a line once per ledger, so an
// extract with the lines of a non-leading ledger beside the leading
// ledger's would be posted twice by the load and summed twice by the
// reconciliation. Which ledger is SAP's leading one is not written down
// in the repository (a question for the SAP specialist, docs/migration.md),
// and a run's scope names no ledger, so nothing filters on a guessed
// identifier: the quality gate and the reconciliation refuse an extract
// whose lines are in more than one ledger, naming the ledgers found, and
// one ledger's lines are taken as they are.

// journalLedgers are the ledgers the extract's journal lines are in,
// sorted; a line without RLDNR counts as the ledger "".
func journalLedgers(e *sap.Extract) []string {
	var out []string
	for _, l := range e.ACDOCA {
		if !slices.Contains(out, l.RLDNR) {
			out = append(out, l.RLDNR)
		}
	}
	slices.Sort(out)
	return out
}

// ledgerIssue is the error that refuses an extract whose journal lines are
// in more than one ledger, and false when they are in one or none.
func ledgerIssue(e *sap.Extract) (Issue, bool) {
	ledgers := journalLedgers(e)
	if len(ledgers) < 2 {
		return Issue{}, false
	}
	quoted := make([]string, len(ledgers))
	for i, l := range ledgers {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	return Issue{Severity: SevError, Table: "ACDOCA", Key: "RLDNR", Message: fmt.Sprintf(
		"the journal lines are in %d ledgers (RLDNR %s): SAP holds a line once per ledger, so they would be posted and summed more than once; extract the leading ledger's lines only",
		len(ledgers), strings.Join(quoted, ", "))}, true
}
