package cashagent_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	contract "github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/cashagent"
)

// cashPaths are the paths of the cash context's operations (ADR-0051), each
// followed in the contract by "/", "?", ":" or nothing.
var cashPaths = []string{"/v1/cash-actuals", "/v1/cash-actual-lines", "/v1/cash-position", "/v1/cash-snapshots",
	"/v1/cash-exports", "/v1/group-liquidity-forecast", "/v1/liquidity-forecast", "/v1/liquidity-forecast-flows",
	"/v1/liquidity-forecast-variance", "/v1/memo-records", "/v1/memo-record-proposals", "/v1/memo-record-uploads"}

// cash tells whether an operation is the cash context's.
func cash(op contract.Operation) bool {
	for _, p := range cashPaths {
		if op.Path == p || strings.HasPrefix(op.Path, p+"/") || strings.HasPrefix(op.Path, p+":") {
			return true
		}
	}
	return false
}

// moving tells whether an operation is one of the writes that pay, post a
// bank statement, block an item or change configuration, which ADR-0051
// §13 says the agent never performs.
func moving(id string) bool {
	return strings.Contains(id, "PaymentRun") || strings.Contains(id, "BankStatement") ||
		strings.Contains(id, "BankFile") || strings.Contains(id, "DunningBlock") || strings.Contains(id, "ChangeRequest")
}

// TestNever: the agent's tools are reads and its two proposals; its never
// list holds every other write of cash, every write of a payment run, a
// bank statement, a bank file, a dunning block and a configuration change
// request, none of them a tool;
// and every write of cash but the proposals is x-agent: false, so the API
// refuses the agent whatever its level (ADR-0051 §13, As built T53i).
func TestNever(t *testing.T) {
	ops, err := contract.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cashagent.Tools {
		t.Run("tool "+id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && !op.Reads && (!slices.Contains(cashagent.Proposers, id) || !op.Proposes) {
				t.Errorf("%s: got %s, want a read or one of the proposals %v", id, op.Method, cashagent.Proposers)
			}
		})
	}
	for id, op := range ops {
		if op.Method == http.MethodGet || op.Reads || slices.Contains(cashagent.Proposers, id) {
			continue
		}
		if (cash(op) || moving(id)) && !slices.Contains(cashagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", cashagent.Never, id)
		}
		if cash(op) && op.AgentAllowed {
			t.Errorf("%s: open to agents, want x-agent: false", id)
		}
	}
	for _, id := range cashagent.Never {
		t.Run("never "+id, func(t *testing.T) {
			if slices.Contains(cashagent.Tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
}
