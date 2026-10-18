package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The fixture's prices: a call of max_tokens n holds, at most, its body
// (about 55 bytes, 19 tokens at the $5 cache write price, 9,500 Micro)
// and n output tokens at $20 a million (2,000 Micro each).

// budget is an agent's monthly budget as identity would give it.
type budget struct {
	minor money.Minor
	cur   string
}

// budgets are the agents' budgets the test gateways read, as
// identity.AgentBudget reads them from agent profiles.
var budgets sync.Map

// budgeted gives the tenant's agent a monthly budget of minor units of cur.
func budgeted(t *testing.T, ten pgtest.Tenant, minor money.Minor, cur string) {
	t.Helper()
	budgets.Store(ten.Agent.ID, budget{minor, cur})
}

// capped sets the tenant's monthly cap, in cents, in its AI policy.
func capped(t *testing.T, ten pgtest.Tenant, cents int64) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id, monthly_cap, cap_currency) VALUES ($1, $2, 'USD')`, ten.ID, cents)
}

// budgetOf reads the budgets above.
func budgetOf(_ context.Context, _ pgx.Tx, agent uuid.UUID) (money.Minor, string, bool, error) {
	b, ok := budgets.Load(agent)
	if !ok {
		return 0, "", false, nil
	}
	return b.(budget).minor, b.(budget).cur, true, nil
}

// call posts a call of maxTokens as the agent.
func call(t *testing.T, g *agents.Gateway, ten pgtest.Tenant, maxTokens int) (int, string, http.Header) {
	t.Helper()
	rec := post(t, g, principal(ten), fmt.Sprintf(`{"model": "erp-default", "max_tokens": %d, "messages": []}`, maxTokens), nil)
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p) // an answer is not a problem; its code is then empty
	return rec.Code, p.Code, rec.Header()
}

// spent is what the line has spent this month: the agent's, or the
// tenant's with agent nil.
func spent(t *testing.T, ten pgtest.Tenant, agent *uuid.UUID) int64 {
	t.Helper()
	var n int64
	pgtest.Scan(t, `SELECT coalesce(sum(spent_micro), 0)::bigint FROM agents.spend WHERE tenant_id = $1 AND principal_id IS NOT DISTINCT FROM $2`,
		[]any{ten.ID, agent}, &n)
	return n
}

// count is the number of rows the query finds for the tenant.
func count(t *testing.T, ten pgtest.Tenant, sql string, args ...any) int {
	t.Helper()
	var n int
	pgtest.Scan(t, sql, append([]any{ten.ID}, args...), &n)
	return n
}

// TestBudgets: a call is held against the agent's monthly budget and the
// tenant's cap, at the most it may cost, before it is forwarded; one that
// does not fit is refused with 429 until the month ends, audited and not
// forwarded; one that does is settled at its cost (ADR-0036).
func TestBudgets(t *testing.T) {
	priced(t, "claude-opus-5-5")
	answer := message("claude-opus-5-5", "end_turn", 10, 1, 0, 0) // 6,000 Micro
	t.Run("a call that fits is answered and settled at its cost", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 3, "USD")
		code, _, _ := call(t, gateway(t, &provider{status: 200, body: answer}), ten, 1000)
		if code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", code)
		}
		if got := spent(t, ten, &ten.Agent.ID); got != 6000 {
			t.Errorf("agent's spend: got %d, want 6000", got)
		}
		if got := spent(t, ten, nil); got != 6000 {
			t.Errorf("tenant's spend: got %d, want 6000", got)
		}
		if n := count(t, ten, `SELECT count(*) FROM agents.model_reservation WHERE tenant_id = $1 AND settled_at IS NULL`); n != 0 {
			t.Errorf("reservations held after the call: got %d, want 0", n)
		}
	})
	t.Run("a call past the budget is refused until the month ends", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 1, "USD")
		p := &provider{status: 200, body: answer}
		g := gateway(t, p)
		g.Now = func() time.Time { return time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC) }
		code, problem, h := call(t, g, ten, 1000)
		if code != http.StatusTooManyRequests || problem != "agents.budget_exhausted" {
			t.Fatalf("got %d %s, want 429 agents.budget_exhausted", code, problem)
		}
		if got := h.Get("Retry-After"); got != "3600" {
			t.Errorf("Retry-After: got %q, want 3600 (the next month)", got)
		}
		if p.sent != nil {
			t.Error("the refused call was forwarded")
		}
		if n := calls(t, ten); n != 0 {
			t.Errorf("ledger rows: got %d, want 0", n)
		}
		if n := count(t, ten, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'agents.budget.refuse' AND outcome = 'refused'
			AND detail->>'line' = 'agent' AND detail->>'month' = '2026-10'`); n != 1 {
			t.Errorf("refusals audited: got %d, want 1", n)
		}
	})
	t.Run("the tenant's cap holds every caller", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		capped(t, ten, 1)
		g := gateway(t, &provider{status: 200, body: answer})
		rec := post(t, g, reqctx.Principal{ID: ten.Service.ID, Kind: reqctx.KindService, TenantID: ten.ID},
			`{"model": "erp-default", "max_tokens": 1000, "messages": []}`, nil)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status: got %d, want 429", rec.Code)
		}
		if n := count(t, ten, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'agents.budget.refuse' AND detail->>'line' = 'tenant'`); n != 1 {
			t.Errorf("refusals on the tenant's line: got %d, want 1", n)
		}
	})
	t.Run("a budget in another currency than the prices' refuses the agent", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 1000, "EUR")
		if code, problem, _ := call(t, gateway(t, &provider{status: 200, body: answer}), ten, 10); code != http.StatusUnprocessableEntity || problem != "agents.budget_currency" {
			t.Errorf("got %d %s, want 422 agents.budget_currency", code, problem)
		}
	})
	t.Run("an unreadable agent budget fails the call", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		g := gateway(t, &provider{status: 200, body: answer})
		g.AgentBudget = func(context.Context, pgx.Tx, uuid.UUID) (money.Minor, string, bool, error) {
			return 0, "", false, errors.New("profile unreadable")
		}
		if code, _, _ := call(t, g, ten, 10); code != http.StatusInternalServerError {
			t.Errorf("status: got %d, want 500", code)
		}
	})
	t.Run("without a budget reader agents are held by the tenant's cap only", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 0, "USD")
		g := gateway(t, &provider{status: 200, body: answer})
		g.AgentBudget = nil
		if code, _, _ := call(t, g, ten, 10); code != http.StatusOK {
			t.Errorf("status: got %d, want 200", code)
		}
	})
	t.Run("a call too large to price is held at the most, past any budget", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 100000, "USD")
		if code, problem, _ := call(t, gateway(t, &provider{status: 200, body: answer}), ten, 1<<62); code != http.StatusTooManyRequests || problem != "agents.budget_exhausted" {
			t.Errorf("got %d %s, want 429 agents.budget_exhausted", code, problem)
		}
	})
	t.Run("a call without max_tokens is refused", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		rec := post(t, gateway(t, &provider{status: 200, body: answer}), principal(ten), `{"model": "erp-default", "messages": []}`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})
}

// TestReservationsExpire: what a call holds is held while it may still be
// in flight; a reservation whose call can no longer settle (the process
// stopped mid-call) frees its line once it expires.
func TestReservationsExpire(t *testing.T) {
	priced(t, "claude-opus-5-5")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		expires time.Time
		want    int
		retry   string
	}{{"held while it may be in flight", now.Add(time.Minute), http.StatusTooManyRequests, "10"}, {"freed once expired", now.Add(-time.Minute), http.StatusOK, ""}} {
		t.Run(c.name, func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			budgeted(t, ten, 3, "USD")
			line := uuid.Must(uuid.NewV7())
			pgtest.Exec(t, `INSERT INTO agents.spend (tenant_id, id, principal_id, month, currency) VALUES ($1, $2, $3, '2026-10-01', 'USD')`, ten.ID, line, ten.Agent.ID)
			pgtest.Exec(t, `INSERT INTO agents.model_reservation (tenant_id, id, spend_id, amount_micro, reserved_at, expires_at) VALUES ($1, $2, $3, 2000000, $4, $5)`,
				ten.ID, uuid.Must(uuid.NewV7()), line, now.Add(-15*time.Minute), c.expires)
			g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)})
			g.Now = func() time.Time { return now }
			code, _, h := call(t, g, ten, 1000)
			if code != c.want {
				t.Errorf("status: got %d, want %d", code, c.want)
			}
			// Held by a call in flight, the budget may have room soon.
			if want := c.retry; h.Get("Retry-After") != want {
				t.Errorf("Retry-After: got %q, want %q", h.Get("Retry-After"), want)
			}
		})
	}
}

// blocking is a provider that holds each call until released, so that
// calls are in flight together.
type blocking struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blocking) Messages(context.Context, []byte, []string) (int, []byte, error) {
	b.entered <- struct{}{}
	<-b.release
	return http.StatusOK, []byte(message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)), nil
}

// TestCallsInFlightHoldTheBudget: calls in flight together cannot spend
// past a budget: each holds its most before it is forwarded, so of four
// calls that each may cost 2 cents against a budget of 5, two are
// forwarded and two refused, whatever their order.
func TestCallsInFlightHoldTheBudget(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	budgeted(t, ten, 5, "USD")
	p := &blocking{entered: make(chan struct{}, 4), release: make(chan struct{})}
	g := gateway(t, p)
	codes := make(chan int, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			code, _, _ := call(t, g, ten, 1000)
			codes <- code
		})
	}
	got := map[int]int{}
	for range 2 { // the two refused answer while the others are in flight
		select {
		case code := <-codes:
			got[code]++
		case <-time.After(30 * time.Second):
			close(p.release)
			t.Fatalf("no call refused while two were in flight: %v", got)
		}
	}
	close(p.release)
	wg.Wait()
	close(codes)
	for code := range codes {
		got[code]++
	}
	if got[http.StatusOK] != 2 {
		t.Errorf("forwarded: got %d, want 2 (%v)", got[http.StatusOK], got)
	}
	if got[http.StatusTooManyRequests] != 2 {
		t.Errorf("refused: got %d, want 2 (%v)", got[http.StatusTooManyRequests], got)
	}
	if n := len(p.entered); n != 2 {
		t.Errorf("calls the provider saw: got %d, want 2", n)
	}
}

// TestBudgetThresholds: a line's 80% and 100% of its cap are each
// announced once a month, with BudgetThresholdReached and an audit entry,
// as the call that reaches them settles; the tenant's line as an agent's.
func TestBudgetThresholds(t *testing.T) {
	priced(t, "claude-opus-5-5")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		tenant bool  // the tenant's line, capped at 2 cents, rather than the agent's
		spent  int64 // before the call, of 2,000,000 Micro
		shown  int16 // the threshold announced before it
		output int64 // tokens the call reports, 2,000 Micro each, besides 4,000 Micro of input
		want   []string
	}{
		{"to 85%", false, 0, 0, 848, []string{"80"}},
		{"to 90%, 80% announced already", false, 1_700_000, 80, 48, nil},
		{"past 100%, 80% announced already", false, 1_800_000, 80, 200, []string{"100"}},
		{"past both at once", false, 0, 0, 1000, []string{"80", "100"}},
		{"the tenant's line", true, 0, 0, 848, []string{"80"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, c.output, 0, 0)})
			g.Now = func() time.Time { return now }
			line, kind := "agent", any(&ten.Agent.ID)
			if c.tenant {
				capped(t, ten, 2)
				line, kind = "tenant", nil
			} else {
				budgeted(t, ten, 2, "USD")
			}
			pgtest.Exec(t, `INSERT INTO agents.spend (tenant_id, id, principal_id, month, currency, spent_micro, threshold) VALUES ($1, $2, $3, '2026-10-01', 'USD', $4, $5)`,
				ten.ID, uuid.Must(uuid.NewV7()), kind, c.spent, c.shown)
			if code, _, _ := call(t, g, ten, 10); code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", code)
			}
			var got []string
			pgtest.Scan(t, `SELECT coalesce(array_agg(payload->>'threshold' ORDER BY (payload->>'threshold')::int), '{}') FROM agents.outbox
				WHERE tenant_id = $1 AND event_type = 'com.example.erp.agents.budget_threshold_reached.v1' AND payload->>'line' = $2`, []any{ten.ID, line}, &got)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("thresholds announced: got %v, want %v", got, c.want)
			}
			if n := count(t, ten, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'agents.budget.threshold' AND detail->>'line' = $2`, line); n != len(c.want) {
				t.Errorf("thresholds audited: got %d, want %d", n, len(c.want))
			}
		})
	}
	t.Run("the event carries the line's cap and spend", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		budgeted(t, ten, 2, "USD")
		g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1000, 0, 0)})
		if code, _, _ := call(t, g, ten, 10); code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", code)
		}
		var payload map[string]any
		pgtest.Scan(t, `SELECT payload FROM agents.outbox WHERE tenant_id = $1 AND payload->>'threshold' = '100'`, []any{ten.ID}, &payload)
		for field, want := range map[string]string{"limit": "map[amount:0.02 currency:USD]", "spent": "map[amount:0.02 currency:USD]", "agent_id": ten.Agent.ID.String()} {
			if got := fmt.Sprint(payload[field]); got != want {
				t.Errorf("%s: got %s, want %s", field, got, want)
			}
		}
	})
}

// TestFailedCallsFreeTheirHold: a call the provider fails is settled at
// nothing, and what it held is free again.
func TestFailedCallsFreeTheirHold(t *testing.T) {
	priced(t, "claude-opus-5-5")
	for name, p := range map[string]*provider{"an error answer": {status: 529, body: `{"type":"error"}`}, "no answer": {err: errors.New("connection reset")}} {
		t.Run(name, func(t *testing.T) {
			ten := pgtest.NewTenant(t)
			budgeted(t, ten, 3, "USD")
			call(t, gateway(t, p), ten, 1000)
			if got := spent(t, ten, &ten.Agent.ID); got != 0 {
				t.Errorf("agent's spend: got %d, want 0", got)
			}
			if n := count(t, ten, `SELECT count(*) FROM agents.model_reservation WHERE tenant_id = $1 AND settled_at IS NULL`); n != 0 {
				t.Errorf("reservations held after the call: got %d, want 0", n)
			}
		})
	}
}

// TestBudgetDatabaseFailures: each statement of a call held against a
// budget, reserved, recorded, settled and announced, failing in turn
// fails the call; so does each of a refusal's.
func TestBudgetDatabaseFailures(t *testing.T) {
	priced(t, "claude-opus-5-5")
	pool := pgtest.Pool(t)
	for _, c := range []struct {
		name   string
		budget money.Minor
		ok     int // the status once no statement fails
	}{{"a call that reaches its cap", 2, http.StatusOK}, {"a refusal", 1, http.StatusTooManyRequests}} {
		t.Run(c.name, func(t *testing.T) {
			for at := 1; ; at++ {
				ten := pgtest.NewTenant(t)
				budgeted(t, ten, c.budget, "USD")
				count := 0
				g := gateway(t, &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1000, 0, 0)})
				g.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx, s)
					return &pgtest.FailingTx{Tx: tx, FailAt: at, Count: &count}, err
				}
				code, _, _ := call(t, g, ten, 500)
				if count < at { // every statement ran: none was failed
					if code != c.ok {
						t.Errorf("with no statement failed: got %d, want %d", code, c.ok)
					}
					return
				}
				if code != http.StatusInternalServerError {
					t.Fatalf("statement %d failed: got %d, want 500", at, code)
				}
			}
		})
	}
}
