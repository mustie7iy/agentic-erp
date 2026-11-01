package app_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
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
	if r.Code != 403 {
		t.Fatalf("agent approval status: got %d, want 403 (%s)", r.Code, r.Body)
	}
	if got := apitest.JSON(t, r)["code"]; got != "identity.agent_not_allowed" {
		t.Errorf("agent approval code: got %v, want identity.agent_not_allowed", got)
	}
	// Bodies are validated against the contract.
	r = api.Do(apitest.Req{Method: "POST", Path: "/v1/legal-entities", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey(), Body: `{"code": 5`})
	if r.Code != 400 {
		t.Fatalf("invalid body status: got %d, want 400 (%s)", r.Code, r.Body)
	}
	if got := apitest.JSON(t, r)["code"]; got != "request.invalid_body" {
		t.Errorf("invalid body code: got %v, want request.invalid_body", got)
	}
	r = api.Do(apitest.Req{Method: "GET", Path: "/v1/nothing-here", Tenant: ten.ID, As: ten.Human})
	if r.Code != 404 {
		t.Fatalf("unknown path status: got %d, want 404 (%s)", r.Code, r.Body)
	}
	if got := apitest.JSON(t, r)["code"]; got != "request.not_found" {
		t.Errorf("unknown path code: got %v, want request.not_found", got)
	}
	r = api.Do(apitest.Req{Method: "PUT", Path: "/v1/operations/01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d0d", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey()})
	if r.Code != 405 {
		t.Fatalf("wrong method status: got %d, want 405 (%s)", r.Code, r.Body)
	}
	if r.Header().Get("Allow") == "" {
		t.Errorf("wrong method Allow: got empty, want the allowed methods (%v)", r.Header())
	}
	if got := apitest.JSON(t, r)["code"]; got != "request.method_not_allowed" {
		t.Errorf("wrong method code: got %v, want request.method_not_allowed", got)
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

// The API logs each country pack that no tax specialist has signed off,
// which the pilot needs, by its directory.
func TestCountryPacksWithoutSignOffAreLogged(t *testing.T) {
	api := apitest.NewWith(t, func(c *app.Config) { c.Packs = apitest.Packs(t, "../configuration/testdata/packs") })
	logs := api.Logs()
	if !strings.Contains(logs, "us/2026.2/pack.yaml: sign_off") {
		t.Errorf("logs: got %q, want the unsigned US 2026.2 pack", logs)
	}
	if strings.Contains(logs, "us/2026.1/pack.yaml") {
		t.Errorf("logs: got %q, want nothing of the signed-off US 2026.1 pack", logs)
	}
}

// Unreleased packs from a directory join the image's, each logged as not
// released (ERP_PACKS_DIR: a demonstration's or a test's).
func TestUnreleasedPacks(t *testing.T) {
	api := apitest.NewWith(t, func(c *app.Config) { c.UnreleasedPacks = "../taxfiling/testdata/packs" })
	if logs := api.Logs(); !strings.Contains(logs, "de/2026.1") || !strings.Contains(logs, "not released") {
		t.Errorf("logs: got %q, want the DE 2026.1 test pack logged as not released", logs)
	}
}
