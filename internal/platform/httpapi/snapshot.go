package httpapi

import (
	"net/http"

	"github.com/mustie7iy/agentic-erp/api"
)

// snapshot reports whether a request reaches an operation the contract
// marks x-snapshot, which reads as of one database state: the unit of work
// runs its transaction at REPEATABLE READ (ADR-0051 §6). The table is
// built from the contract, so no list is kept by hand.
func snapshot(routes router, ops map[string]api.Operation) func(*http.Request) bool {
	marked := map[string]bool{}
	for id, op := range ops {
		if op.Snapshot {
			marked[id] = true
		}
	}
	return func(r *http.Request) bool {
		route, ok := routes.FindPath(r.Method, r.URL)
		return ok && marked[route.OperationID()]
	}
}
