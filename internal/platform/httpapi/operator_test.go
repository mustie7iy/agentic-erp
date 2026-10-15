package httpapi_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestOperatorServe: an operation runs for an operator with its role, is
// refused 403 without it, and its errors are problems, the platform's
// failures logged under the API's name.
func TestOperatorServe(t *testing.T) {
	var logged bytes.Buffer
	op := httpapi.Operator{Roles: map[string]string{"list": authn.RoleRead}, Log: slog.New(slog.NewTextHandler(&logged, nil)), Name: "control"}
	ok := func(w http.ResponseWriter, _ *http.Request) error {
		httpapi.WriteJSON(w, http.StatusOK, map[string]int{"n": 1})
		return nil
	}
	conflict := func(http.ResponseWriter, *http.Request) error { return problem.Conflict("x.conflict", "No.") }
	broken := func(http.ResponseWriter, *http.Request) error { return errors.New("disk on fire") }
	cases := map[string]struct {
		roles     []string
		operation string
		body      func(http.ResponseWriter, *http.Request) error
		status    int
		logged    bool
	}{
		"reader reads":            {[]string{authn.RoleRead}, "list", ok, 200, false},
		"reader may not operate":  {[]string{authn.RoleRead}, "suspend", ok, 403, false},
		"operator operates":       {[]string{authn.RoleOperate}, "suspend", ok, 200, false},
		"no roles: everything":    {nil, "suspend", ok, 200, false},
		"a client error":          {nil, "suspend", conflict, 409, false},
		"a failure of the server": {nil, "suspend", broken, 500, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			logged.Reset()
			ctx, info := reqctx.WithInfo(reqctx.WithClaims(t.Context(), reqctx.Claims{Roles: c.roles}))
			w := httptest.NewRecorder()
			op.Serve(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), c.operation, c.body)
			if w.Code != c.status {
				t.Errorf("status: got %d, want %d: %s", w.Code, c.status, w.Body)
			}
			if info.Operation != c.operation {
				t.Errorf("operation: got %q, want %q", info.Operation, c.operation)
			}
			if got := strings.Contains(logged.String(), "control request failed"); got != c.logged {
				t.Errorf("logged: got %v, want %v: %s", got, c.logged, logged.String())
			}
		})
	}
}

func TestOperatorRequired(t *testing.T) {
	op := httpapi.Operator{Roles: map[string]string{"create": authn.RoleProvision}}
	if got := op.Required("create"); got != authn.RoleProvision {
		t.Errorf("listed operation: got %s, want %s", got, authn.RoleProvision)
	}
	if got := op.Required("erase"); got != authn.RoleOperate {
		t.Errorf("other operation: got %s, want %s", got, authn.RoleOperate)
	}
}

func TestNotAllowed(t *testing.T) {
	w := httptest.NewRecorder()
	httpapi.NotAllowed(w, httptest.NewRequest("PUT", "/", nil), "GET, POST")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow: got %q, want %q", got, "GET, POST")
	}
	if !strings.Contains(w.Body.String(), "request.method_not_allowed") {
		t.Errorf("body: got %s, want the problem request.method_not_allowed", w.Body)
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	httpapi.WriteJSON(w, http.StatusCreated, map[string]string{"id": "x"})
	if w.Code != http.StatusCreated {
		t.Errorf("status: got %d, want 201", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"id":"x"}` {
		t.Errorf("body: got %s, want {\"id\":\"x\"}", got)
	}
}

// TestOperatorServeWithoutClaims: a request that reached Serve without the
// authenticating middleware carries no claims. Their zero value has nil
// roles, which reads as a development token holding every role, so the
// request is refused instead.
func TestOperatorServeWithoutClaims(t *testing.T) {
	op := httpapi.Operator{Log: slog.New(slog.DiscardHandler), Name: "control"}
	ran := false
	ctx, _ := reqctx.WithInfo(t.Context())
	w := httptest.NewRecorder()
	op.Serve(w, httptest.NewRequestWithContext(ctx, "GET", "/", nil), "suspend", func(http.ResponseWriter, *http.Request) error {
		ran = true
		return nil
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d: %s", w.Code, http.StatusForbidden, w.Body)
	}
	if ran {
		t.Error("ran: got true, want the operation refused before it runs")
	}
}
