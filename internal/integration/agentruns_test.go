package integration_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// TestAgentRehearsesOnly: an agent creates and starts only rehearsals: a
// run that would stay loaded is refused with identity.agent_not_allowed,
// whether the agent asks for it or a person created it, before anything
// is loaded; a rehearsal it starts as a person would (review:batch76).
func TestAgentRehearsesOnly(t *testing.T) {
	e := setup(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	agent := apitest.Client{T: t, API: e.API, Tenant: e.Tenant, As: e.Tenant.Agent}
	conn := e.connector(t, "extract.json")
	body := func(rehearsal bool) string {
		return fmt.Sprintf(`{"connector_id": %q, "scope": %s, "mode": "documents", "is_rehearsal": %t}`, conn, scope(e.DE.String()), rehearsal)
	}
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/migration-runs", body(false), "")
	persons := e.run(t, conn, "documents", false, e.DE.String())
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/migration-runs/"+persons["id"].(string)+":start", "", `"1"`)
	if got, _ := e.Must(200, "GET", "/v1/migration-runs/"+persons["id"].(string), "", ""); got["status"] != "queued" {
		t.Errorf("the person's run: got %v, want queued", got["status"])
	}
	rehearsal, _ := agent.Must(201, "POST", "/v1/migration-runs", body(true), "")
	agent.Must(202, "POST", "/v1/migration-runs/"+rehearsal["id"].(string)+":start", "", `"1"`)
}
