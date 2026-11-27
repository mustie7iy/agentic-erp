package controllingagent_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/controllingagent"
)

// controllingPaths are the paths of the controlling operations (ADR-0050,
// ADR-0064): the centers, segments and hierarchies the ledger keeps, their
// changes and reports, and the controlling context's plans, orders,
// allocations and key figures; each followed in the contract by "/",
// "?", ":", "-" or nothing.
var controllingPaths = []string{"/v1/cost-center", "/v1/profit-center", "/v1/segments", "/v1/center-changes", "/v1/hierarchies",
	"/v1/plan-categories", "/v1/plan-batches", "/v1/plan-lines", "/v1/internal-orders", "/v1/allocation-cycles", "/v1/allocation-runs", "/v1/statistical-key-figures"}

// controlling tells whether an operation is one of controlling's.
func controlling(op api.Operation) bool {
	return slices.ContainsFunc(controllingPaths, func(p string) bool { return op.Path == p || strings.HasPrefix(op.Path, p) })
}

// covered tells whether a write is one the never list must hold: every
// write of controlling, of a journal entry parked or posted, of an entry
// template, of a fiscal period, of an approval and of a configuration
// change request.
func covered(op api.Operation) bool {
	return controlling(op) || slices.ContainsFunc([]string{"JournalEntr", "EntryTemplate", "FiscalPeriod", "ApprovalRequest", "ChangeRequest"},
		func(s string) bool { return strings.Contains(op.ID, s) })
}

// TestNever: the model's tools are reads and the agent's own proposer;
// its code's one write, parkJournalEntry, is a proposal; the never list
// holds every other write of controlling, of a journal entry, an entry
// template, a fiscal period, an approval and a change request, none of
// them a tool; and posting a parked entry, reversing, a decision of an
// approval and a lock of a center or a plan category are x-agent: false,
// so the API refuses the agent whatever its level, as the ledger's code
// refuses it postJournalEntry (ADR-0050 §12, T56j1).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range controllingagent.Reads {
		t.Run("read "+id, func(t *testing.T) {
			if op, ok := ops[id]; !ok || op.Method != http.MethodGet {
				t.Errorf("%s: got %s, want a read of the contract", id, op.Method)
			}
		})
	}
	if op := ops[controllingagent.Parker]; !op.Proposes || !op.AgentAllowed {
		t.Errorf("%s: got proposes %v, open to agents %v, want a proposal open to agents", controllingagent.Parker, op.Proposes, op.AgentAllowed)
	}
	if _, ok := ops[controllingagent.Proposer]; ok {
		t.Errorf("%s: an operation of the contract, want the agent's own tool", controllingagent.Proposer)
	}
	for id, op := range ops {
		if op.Method != http.MethodGet && id != controllingagent.Parker && covered(op) && !slices.Contains(controllingagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", controllingagent.Never, id)
		}
	}
	for _, id := range controllingagent.Never {
		t.Run("never "+id, func(t *testing.T) {
			if slices.Contains(controllingagent.Reads, id) || id == controllingagent.Parker || id == controllingagent.Proposer {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
	for _, id := range []string{"reverseJournalEntry", "postParkedJournalEntry", "approveApprovalRequest", "rejectApprovalRequest",
		"lockCostCenter", "lockProfitCenter", "lockPlanCategory", "unlockPlanCategory"} {
		t.Run("people only "+id, func(t *testing.T) {
			if ops[id].AgentAllowed {
				t.Errorf("%s: open to agents, want x-agent: false", id)
			}
		})
	}
}
