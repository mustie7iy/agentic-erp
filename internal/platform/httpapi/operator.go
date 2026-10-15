package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Operator serves the operations of a hand-written operator API: the
// control plane (api/control.yaml) and the global directory
// (api/directory.yaml), which run behind authn's OperatorMiddleware rather
// than the generated server. Each operation needs an operator role
// (ADR-0027), and errors are RFC 9457 problems like the public API's.
type Operator struct {
	// Roles maps an operation to the role it needs; any other operation
	// needs authn.RoleOperate.
	Roles map[string]string
	// Log receives the platform's failures (status 500 and above) as
	// "<Name> request failed".
	Log  *slog.Logger
	Name string
}

// Required is the role an operation needs: authn.RoleOperate unless Roles
// says otherwise.
func (o Operator) Required(operation string) string {
	if role, ok := o.Roles[operation]; ok {
		return role
	}
	return authn.RoleOperate
}

// Serve runs one operation for an operator holding its role: its body
// answers, or fails with an error sent as a problem.
func (o Operator) Serve(w http.ResponseWriter, r *http.Request, operation string, body func(http.ResponseWriter, *http.Request) error) {
	reqctx.InfoFrom(r.Context()).Operation = operation
	// No claims means no authenticating middleware ran: refused, since the
	// zero Claims (nil roles) would read as a development token's.
	if claims, ok := reqctx.ClaimsFrom(r.Context()); !ok || !authn.OperatorAllowed(claims, o.Required(operation)) {
		problem.Write(w, r, problem.Forbidden("authz.operator_role", "This operation needs the operator role "+o.Required(operation)+"."))
		return
	}
	if err := body(w, r); err != nil {
		p := problem.From(err)
		if p.Status >= 500 {
			o.Log.ErrorContext(r.Context(), o.Name+" request failed", "operation", operation, "error", err, "trace_id", reqctx.TraceID(r.Context()))
		}
		problem.Write(w, r, p)
	}
}

// NotAllowed answers 405 with the methods the resource allows.
func NotAllowed(w http.ResponseWriter, r *http.Request, allowed string) {
	w.Header().Set("Allow", allowed)
	problem.Write(w, r, problem.New(http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed", "Allowed: "+allowed+"."))
}

// WriteJSON answers with v as JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status is sent; a failed write cannot be reported
}
