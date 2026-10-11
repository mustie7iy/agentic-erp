package httpapi_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestTheWholeContractIsServed(t *testing.T) {
	api := apitest.New(t)
	ten := pgtest.NewTenant(t, "integration.migration.read", "tenancy.org.manage", "workflow.request.decide")
	// Every operation of the contract is served.
	r := api.Do(apitest.Req{Method: "GET", Path: "/v1/migration-runs", Tenant: ten.ID, As: ten.Human})
	if r.Code != 200 {
		t.Fatalf("migration runs: %d %s", r.Code, r.Body)
	}
	// Approval decisions refuse agents before anything else.
	r = api.Do(apitest.Req{Method: "POST", Path: "/v1/approval-requests/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d0d:approve",
		Tenant: ten.ID, As: ten.Agent, Key: apitest.NewKey(), IfMatch: `"1"`, Body: `{}`})
	if r.Code != 403 || apitest.JSON(t, r)["code"] != "identity.agent_not_allowed" {
		t.Fatalf("agent approval: %d %s", r.Code, r.Body)
	}
	// Bodies are validated against the contract.
	r = api.Do(apitest.Req{Method: "POST", Path: "/v1/legal-entities", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey(), Body: `{"code": 5`})
	if r.Code != 400 || apitest.JSON(t, r)["code"] != "request.invalid_body" {
		t.Fatalf("invalid body: %d %s", r.Code, r.Body)
	}
	r = api.Do(apitest.Req{Method: "GET", Path: "/v1/nothing-here", Tenant: ten.ID, As: ten.Human})
	if r.Code != 404 || apitest.JSON(t, r)["code"] != "request.not_found" {
		t.Fatalf("unknown path: %d %s", r.Code, r.Body)
	}
	r = api.Do(apitest.Req{Method: "PUT", Path: "/v1/operations/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d0d", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey()})
	if r.Code != 405 || r.Header().Get("Allow") == "" || apitest.JSON(t, r)["code"] != "request.method_not_allowed" {
		t.Fatalf("wrong method: %d %v %s", r.Code, r.Header(), r.Body)
	}
	// Every request is logged and measured with its operation.
	if !strings.Contains(api.Logs(), `"operation":"listMigrationRuns"`) {
		t.Fatal("operation not logged")
	}
	m := httptest.NewRecorder()
	api.Metrics.Handler().ServeHTTP(m, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(m.Body.String(), `operation="listMigrationRuns"`) {
		t.Fatal("operation not measured")
	}
}
