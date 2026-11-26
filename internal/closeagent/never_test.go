package closeagent_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/closeagent"
)

// closingPaths are the paths of the closing context's operations (ADR-0058
// §13), each followed in the contract by "/", "?", ":" or nothing.
var closingPaths = []string{"/v1/period-closes", "/v1/year-closes", "/v1/close-tasks", "/v1/close-calendar",
	"/v1/accrual-proposals", "/v1/variance-comment-proposals", "/v1/variance-comments"}

// closing tells whether an operation is the closing context's.
func closing(op api.Operation) bool {
	for _, p := range closingPaths {
		if op.Path == p || strings.HasPrefix(op.Path, p+"/") || strings.HasPrefix(op.Path, p+":") {
			return true
		}
	}
	return false
}

// proposing are the agent's two proposals, the only writes among its tools.
var proposing = []string{"proposeAccrual", "proposeVarianceComment"}

// TestNever: the agent's tools are reads and its two proposals; its never
// list holds every other write of closing, every write of a journal entry
// or a fiscal period and every decision of an approval, none of them a
// tool; and every write of closing but the proposals is x-agent: false, so
// the API refuses the agent whatever its level (ADR-0058 §12).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := slices.Concat(closeagent.AccrualTools, closeagent.VarianceTools)
	for _, id := range tools {
		t.Run("tool "+id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && (!slices.Contains(proposing, id) || !op.Proposes) {
				t.Errorf("%s: got %s, want a read or one of the proposals %v", id, op.Method, proposing)
			}
		})
	}
	for id, op := range ops {
		if op.Method == http.MethodGet || slices.Contains(proposing, id) {
			continue
		}
		covered := closing(op) || strings.Contains(id, "JournalEntr") || strings.Contains(id, "FiscalPeriod") || strings.Contains(id, "ApprovalRequest")
		if covered && !slices.Contains(closeagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", closeagent.Never, id)
		}
		if closing(op) && op.AgentAllowed {
			t.Errorf("%s: open to agents, want x-agent: false", id)
		}
	}
	for _, id := range closeagent.Never {
		t.Run("never "+id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
}
