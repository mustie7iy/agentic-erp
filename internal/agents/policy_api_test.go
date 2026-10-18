package agents_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// policyAdmin is a person who may read and change the tenant's AI policy,
// and the tenant's agent.
func policyAdmin(t *testing.T) (apitest.Client, apitest.Client) {
	t.Helper()
	ten := pgtest.NewTenant(t, "agents.ai_policy.read", "agents.ai_policy.manage")
	person := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	agent := person
	agent.As = ten.Agent
	return person, agent
}

// TestAIPolicyAPI: the tenant's administrators read their AI policy, the
// defaults until they change it, and change it by merge patch, audited and
// published; agents may do neither.
func TestAIPolicyAPI(t *testing.T) {
	priced(t, "claude-opus-5-5")
	priced(t, "claude-haiku-5")
	person, agent := policyAdmin(t)
	got, tag := person.Must(200, "GET", "/v1/ai-policy", "", "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if got["active"] != true {
		t.Errorf("active: got %v, want true", got["active"])
	}
	if routes := apitest.Object(t, got, "effective_routes"); routes["erp-extraction"] != agents.DefaultRoutes["erp-extraction"] {
		t.Errorf("effective routes: got %v, want the platform's", routes)
	}
	agent.Problem(403, "identity.agent_not_allowed", "GET", "/v1/ai-policy", "", "")
	agent.Problem(403, "identity.agent_not_allowed", "PATCH", "/v1/ai-policy", `{"enabled": true}`, `"1"`)

	got, tag = person.Must(200, "PATCH", "/v1/ai-policy", `{"enabled": false, "monthly_cap": {"amount": "500.00", "currency": "USD"},
		"routes": {"erp-default": "claude-haiku-5"}, "allowed_models": ["claude-haiku-5", "claude-opus-5-5"], "documents_to_models": false}`, `"1"`)
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	if got["active"] != false {
		t.Errorf("active: got %v, want false", got["active"])
	}
	if c := fmt.Sprint(got["monthly_cap"]); c != "map[amount:500.00 currency:USD]" {
		t.Errorf("cap: got %s, want 500.00 USD", c)
	}
	if routes := apitest.Object(t, got, "effective_routes"); routes["erp-default"] != "claude-haiku-5" {
		t.Errorf("effective route: got %v, want claude-haiku-5", routes["erp-default"])
	}
	if got["updated_by"] != person.As.ID.String() {
		t.Errorf("updated_by: got %v, want %s", got["updated_by"], person.As.ID)
	}
	ev := apitest.Published(t, person.Tenant, outbox.Event{Context: "agents", Name: "ai_policy_changed"})
	if len(ev) != 1 {
		t.Fatalf("events: got %d, want 1", len(ev))
	}
	if f := fmt.Sprint(ev[0]["changed_fields"]); f != "[allowed_models documents_to_models enabled monthly_cap routes]" {
		t.Errorf("changed fields: got %s", f)
	}
	if n := count(t, person.Tenant, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'agents.ai_policy.update'
		AND detail->>'active' = 'false'`); n != 1 {
		t.Errorf("audit entries: got %d, want 1", n)
	}

	got, _ = person.Must(200, "PATCH", "/v1/ai-policy", `{"monthly_cap": null, "routes": {}, "allowed_models": []}`, `"2"`)
	if _, ok := got["monthly_cap"]; ok {
		t.Errorf("cap: got %v, want none", got["monthly_cap"])
	}
	if got["enabled"] != false {
		t.Errorf("enabled: got %v, want false, unchanged", got["enabled"])
	}
	person.Problem(412, "request.version_mismatch", "PATCH", "/v1/ai-policy", `{"enabled": true}`, `"2"`)
}

// TestAIPolicyAPIRefusals: a cap in another currency or at the wrong
// precision, an unknown alias and a route outside the allowed models are
// refused and change nothing.
func TestAIPolicyAPIRefusals(t *testing.T) {
	priced(t, "claude-opus-5-5")
	person, _ := policyAdmin(t)
	for name, body := range map[string]string{
		"a cap in euros":        `{"monthly_cap": {"amount": "5.00", "currency": "EUR"}}`,
		"a cap with 3 decimals": `{"monthly_cap": {"amount": "5.001", "currency": "USD"}}`,
		"an unknown alias":      `{"routes": {"erp-other": "claude-opus-5-5"}}`,
		"a route not allowed":   `{"allowed_models": ["claude-haiku-5"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			person.In(t).Problem(422, "agents.invalid_policy", "PATCH", "/v1/ai-policy", body, `"1"`)
		})
	}
	if _, tag := person.Must(200, "GET", "/v1/ai-policy", "", ""); tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\", unchanged", tag)
	}
}

// TestAIPolicyAPIDatabaseFailures: every statement of reading and changing
// the policy through the API failing in turn fails the request.
func TestAIPolicyAPIDatabaseFailures(t *testing.T) {
	priced(t, "claude-opus-5-5")
	person, _ := policyAdmin(t)
	capped(t, person.Tenant, 100) // a cap, so that reading it fails too
	h := &agents.Endpoints{}
	for name, call := range map[string]func(ctx context.Context) error{
		"get": func(ctx context.Context) error {
			_, err := h.GetAiPolicy(ctx)
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateAiPolicy(ctx, &gen.AiPolicyUpdate{Enabled: gen.NewOptBool(false),
				MonthlyCap: gen.NewOptNilAiPolicyUpdateMonthlyCap(gen.AiPolicyUpdateMonthlyCap{Amount: "1.00", Currency: "USD"})},
				gen.UpdateAiPolicyParams{IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Errorf("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, person.Tenant, person.As, name, call)
		})
	}
}
