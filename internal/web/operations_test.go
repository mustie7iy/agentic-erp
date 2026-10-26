package web

import (
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// specOperation is what the contract says of an operation that the
// workspace's inventory is held to.
type specOperation struct {
	ID                string            `yaml:"operationId"`
	Permission        string            `yaml:"x-permission"`
	Agent             *bool             `yaml:"x-agent"`
	ObjectPermissions map[string]string `yaml:"x-object-permissions"`
	ProblemCodes      []string          `yaml:"x-problem-codes"`
}

// contract reads the operations of api/openapi.yaml by operationId, and
// the codes the request pipeline may answer on any of them.
func contract(t *testing.T) (map[string]specOperation, []string) {
	t.Helper()
	var doc struct {
		Codes []string                        `yaml:"x-problem-codes"`
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]specOperation{}
	for _, methods := range doc.Paths {
		for _, node := range methods {
			var op specOperation
			if err := node.Decode(&op); err == nil && op.ID != "" {
				ops[op.ID] = op
			}
		}
	}
	if len(ops) < 100 || len(doc.Codes) == 0 {
		t.Fatalf("contract: %d operations and %d document codes read, too few to judge", len(ops), len(doc.Codes))
	}
	return ops, doc.Codes
}

// TestScreensAgainstTheContract is ADR-0052 §10's contract-drift test:
// every operation a screen calls is in the contract, the permission a
// screen hides an action by is that operation's x-permission, an x-agent:
// false operation is called only from a human-only route, the decisions'
// approval permissions are their x-object-permissions, every route in the
// inventory is the workspace's, and every problem code the catalog
// explains is one the screens' operations can answer.
func TestScreensAgainstTheContract(t *testing.T) {
	ops, codes := contract(t)
	s, _ := testServer(t)
	mux := s.mux()
	answerable := map[string]bool{}
	for _, c := range codes {
		answerable[c] = true
	}
	for route, calls := range Screens {
		t.Run(route, func(t *testing.T) {
			method, path, _ := strings.Cut(route, " ")
			r := httptest.NewRequest(method, strings.NewReplacer("{$}", "", "{id}", "0192", "{rec}", "0193").Replace(path), nil)
			if _, pattern := mux.Handler(r); pattern != route {
				t.Errorf("the workspace serves %s %s as %q, want the inventory's %q", method, r.URL.Path, pattern, route)
			}
			// ADR-0052 §10: at most six API calls render a page; a form's
			// own write comes first and is not one of them.
			reads := calls
			if method == "POST" && len(calls) > 1 {
				reads = calls[1:]
			}
			// Nor are the lookups that turn a form's codes into the
			// write's ids (T73h): a page of each list, and at most
			// maxCodeLookups codes one by one, so a send with its page's
			// reads stays well within the API's 50 calls a second for a
			// principal (04 §4.11).
			lookups := slices.DeleteFunc(slices.Clone(reads), func(op Operation) bool { return !op.Lookup })
			reads = slices.DeleteFunc(slices.Clone(reads), func(op Operation) bool { return op.Lookup })
			if n := len(reads) + len(lookups) + maxCodeLookups; len(lookups) > 0 && n > 40 {
				t.Errorf("%d calls at most for a send with its lookups, want at most 40 of the API's 50 a second", n)
			}
			if len(reads) > 6 {
				t.Errorf("%d calls to render the page, want at most six", len(reads))
			}
			for _, call := range calls {
				op, ok := ops[call.ID]
				if !ok {
					t.Errorf("%s: no such operation in the contract", call.ID)
					continue
				}
				if call.HiddenBy != "" && call.HiddenBy != op.Permission {
					t.Errorf("%s: hidden by %s, want its x-permission %s", call.ID, call.HiddenBy, op.Permission)
				}
				if op.Agent != nil && !*op.Agent && !call.HumanOnly {
					t.Errorf("%s: x-agent: false, called from a route not marked human-only", call.ID)
				}
				for _, c := range op.ProblemCodes {
					answerable[c] = true
				}
			}
		})
	}
	for _, id := range []string{"approveApprovalRequest", "rejectApprovalRequest"} {
		if got := ops[id].ObjectPermissions; !maps.Equal(got, objectApprovers) {
			t.Errorf("%s: x-object-permissions %v, want the workspace's %v", id, got, objectApprovers)
		}
	}
	for _, explained := range []map[string]i18n.Key{i18n.Problems, i18n.InvoiceProblems, i18n.BankProblems, i18n.PaymentProblems,
		i18n.PeriodProblems, i18n.RevaluationProblems, i18n.EntryProblems} {
		for _, code := range slices.Sorted(maps.Keys(explained)) {
			if !answerable[code] {
				t.Errorf("problem %s: explained by the catalog, but no operation the screens call answers it", code)
			}
		}
	}
	for kind := range objectApprovers {
		if _, ok := objectKinds[kind]; !ok {
			t.Errorf("object type %s: no name in the catalog", kind)
		}
	}
}
