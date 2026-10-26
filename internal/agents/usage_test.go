package agents_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// called records a model call of principal at, costing micro.
func called(t *testing.T, ten pgtest.Tenant, principal uuid.UUID, at string, micro int64, redactions int) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cache_write_tokens,
		cache_read_tokens, cost_micro, currency, redactions, called_at)
		VALUES ($1, gen_random_uuid(), $2, 'erp-default', 'claude-opus-5-5', 'succeeded', 100, 10, 5, 1, $3, 'USD', $4, $5)`, ten.ID, principal, micro, redactions, at)
}

// TestAIUsage: a month's usage is its calls' per principal, the most
// costly first, with the month's total and the tenant's cap; another
// month's calls and another tenant's stay out.
func TestAIUsage(t *testing.T) {
	person, agent := policyAdmin(t)
	ten := person.Tenant
	capped(t, ten, 50000)
	called(t, ten, ten.Agent.ID, "2026-10-01T00:00:00Z", 1_500_000, 2) // 1.5 cents
	called(t, ten, ten.Agent.ID, "2026-10-31T23:59:59Z", 1_000_000, 1)
	called(t, ten, ten.Human.ID, "2026-10-15T12:00:00Z", 400_000, 0)
	called(t, ten, ten.Agent.ID, "2026-11-01T00:00:00Z", 9_000_000, 0) // next month
	other := pgtest.NewTenant(t)
	called(t, other, other.Agent.ID, "2026-10-10T00:00:00Z", 7_000_000, 0)

	got, _ := person.Must(200, "GET", "/v1/ai-usage?month=2026-10", "", "")
	if got["calls"] != float64(3) {
		t.Errorf("calls: got %v, want 3", got["calls"])
	}
	if c := fmt.Sprint(got["cost"]); c != "map[amount:0.03 currency:USD]" {
		t.Errorf("cost: got %s, want 0.03 USD (2.9 cents)", c)
	}
	if c := fmt.Sprint(got["monthly_cap"]); c != "map[amount:500.00 currency:USD]" {
		t.Errorf("cap: got %s, want 500.00 USD", c)
	}
	lines := apitest.List(t, got, "principals")
	if len(lines) != 2 {
		t.Fatalf("principals: got %d, want 2", len(lines))
	}
	first := lines[0].(map[string]any)
	if first["principal_id"] != ten.Agent.ID.String() {
		t.Errorf("first: got %v, want the agent, the most costly", first["principal_id"])
	}
	for field, want := range map[string]any{"calls": float64(2), "input_tokens": float64(200), "output_tokens": float64(20),
		"cache_write_tokens": float64(10), "cache_read_tokens": float64(2), "redactions": float64(3)} {
		if first[field] != want {
			t.Errorf("agent's %s: got %v, want %v", field, first[field], want)
		}
	}
	if c := fmt.Sprint(first["cost"]); c != "map[amount:0.03 currency:USD]" {
		t.Errorf("agent's cost: got %s, want 0.03 USD (2.5 cents)", c)
	}
	empty, _ := person.Must(200, "GET", "/v1/ai-usage?month=2025-01", "", "")
	if n := len(apitest.List(t, empty, "principals")); n != 0 {
		t.Errorf("an empty month's principals: got %d, want 0", n)
	}
	if empty["calls"] != float64(0) {
		t.Errorf("an empty month's calls: got %v, want 0", empty["calls"])
	}
	second := lines[1].(map[string]any)
	if second["principal_id"] != ten.Human.ID.String() {
		t.Errorf("second: got %v, want the person whose request called", second["principal_id"])
	}
	if c := fmt.Sprint(second["cost"]); c != "map[amount:0.00 currency:USD]" {
		t.Errorf("person's cost: got %s, want 0.00 USD (0.4 cent)", c)
	}
	person.Problem(400, "request.invalid_parameter", "GET", "/v1/ai-usage?month=2026-13", "", "")
	agent.Problem(403, "identity.agent_not_allowed", "GET", "/v1/ai-usage", "", "")
}

// TestAIUsageDefaultsToThisMonth: without a month, the current one (UTC).
func TestAIUsageDefaultsToThisMonth(t *testing.T) {
	person, _ := policyAdmin(t)
	called(t, person.Tenant, person.Tenant.Agent.ID, "2026-03-31T23:59:59Z", 1_000_000, 0)
	called(t, person.Tenant, person.Tenant.Agent.ID, "2026-04-01T00:00:00Z", 1_000_000, 0) // the next month
	h := &agents.Endpoints{Now: func() time.Time { return time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) }}
	var res gen.GetAiUsageRes
	if err := apitest.InRequest(t, person.Tenant, person.As, func(ctx context.Context) (err error) {
		res, err = h.GetAiUsage(ctx, gen.GetAiUsageParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if u := res.(*gen.AiUsage); u.Calls != 1 {
		t.Errorf("calls this month: got %d, want 1", u.Calls)
	}
}

// TestAIUsageDatabaseFailures: every statement failing fails the read.
func TestAIUsageDatabaseFailures(t *testing.T) {
	person, _ := policyAdmin(t)
	capped(t, person.Tenant, 100)
	called(t, person.Tenant, person.Tenant.Agent.ID, "now", 1_000_000, 0)
	h := &agents.Endpoints{}
	call := func(ctx context.Context) error {
		_, err := h.GetAiUsage(ctx, gen.GetAiUsageParams{Month: gen.NewOptString("2026-10")})
		return err
	}
	if call(context.Background()) == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, person.Tenant, person.As, "usage", call)
}

// TestAIUsageHeldAtTheMost: tokens and costs the provider reported past
// what an int64 sums to are held at the most, as a call's cost is, never
// failing the month's report.
func TestAIUsageHeldAtTheMost(t *testing.T) {
	person, _ := policyAdmin(t)
	for range 2 {
		pgtest.Exec(t, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cache_write_tokens,
			cache_read_tokens, cost_micro, currency, called_at)
			VALUES ($1, gen_random_uuid(), $2, 'erp-default', 'm', 'succeeded', $3, $3, $3, $3, $3, 'USD', '2026-10-05')`,
			person.Tenant.ID, person.Tenant.Agent.ID, int64(math.MaxInt64))
	}
	got, _ := person.Must(200, "GET", "/v1/ai-usage?month=2026-10", "", "")
	line := apitest.List(t, got, "principals")[0].(map[string]any)
	if line["input_tokens"] != float64(math.MaxInt64) {
		t.Errorf("input tokens: got %v, want the most an int64 holds", line["input_tokens"])
	}
}

// TestAIUsageReadsOnlyItsMonth: a month's report reads that month's calls,
// not the tenant's whole ledger (model_call_by_time).
func TestAIUsageReadsOnlyItsMonth(t *testing.T) {
	person, _ := policyAdmin(t)
	ten := person.Tenant
	pgtest.Exec(t, `INSERT INTO agents.model_call (tenant_id, id, principal_id, alias, model, status, input_tokens, output_tokens, cost_micro, currency, called_at)
		SELECT $1, gen_random_uuid(), gen_random_uuid(), 'erp-default', 'm', 'succeeded', 1, 1, 1, 'USD', timestamptz '2024-01-01' + g * interval '1 hour'
		FROM generate_series(1, 2000) g`, ten.ID)
	called(t, ten, ten.Agent.ID, "2026-10-05T00:00:00Z", 1, 0)
	pgtest.Exec(t, `ANALYZE agents.model_call`)
	h := &agents.Endpoints{}
	params := gen.GetAiUsageParams{Month: gen.NewOptString("2026-10")}
	if err := apitest.InRequest(t, ten, person.As, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx) // InRequest always puts its transaction in the context
		before := pgtest.RowsRead(t, ctx, tx, "agents", "model_call")
		if _, err := h.GetAiUsage(ctx, params); err != nil {
			return err
		}
		if got := pgtest.RowsRead(t, ctx, tx, "agents", "model_call") - before; got != 1 {
			t.Errorf("calls read for a month with one: got %d, want 1", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
