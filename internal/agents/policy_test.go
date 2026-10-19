package agents_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// inTenant runs fn in the tenant's scope as the application and commits
// when it succeeds.
func inTenant(t *testing.T, ten pgtest.Tenant, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	return pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, fn)
}

// codeOf is a problem's code, with its first field error's code after a
// slash, or the error's text.
func codeOf(err error) string {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return fmt.Sprint(err)
	}
	if len(p.Errors) > 0 {
		return p.Code + "/" + p.Errors[0].Code
	}
	return p.Code
}

// TestAIPolicyDefaults: a tenant that never changed its policy has AI on,
// the platform's routes, any model, no cap and documents allowed.
func TestAIPolicyDefaults(t *testing.T) {
	ten := pgtest.NewTenant(t)
	var p agents.Policy
	if err := inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = agents.ReadPolicy(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"active", p.Active(), true}, {"documents to models", p.DocumentsToModels, true}, {"cap", p.MonthlyCap == nil, true},
		{"routes", len(p.Routes), 0}, {"allowed models", len(p.AllowedModels), 0}, {"version", p.Version, int32(1)},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if model, _ := p.Route("erp-default", agents.DefaultRoutes); model != agents.DefaultRoutes["erp-default"] {
		t.Errorf("route: got %s, want the platform's %s", model, agents.DefaultRoutes["erp-default"])
	}
}

// TestUpdateAIPolicy: the tenant's administrators change the policy as a
// merge patch, guarded by its version; routes name the platform's aliases
// and priced models within the allowed ones.
func TestUpdateAIPolicy(t *testing.T) {
	priced(t, "claude-opus-5-5")
	priced(t, "claude-haiku-5")
	ten := pgtest.NewTenant(t)
	update := func(ifMatch string, ch agents.PolicyChange) (agents.Policy, error) {
		var p agents.Policy
		err := inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) (err error) {
			p, err = agents.UpdatePolicy(ctx, tx, ten.Human.ID, etag.IfMatch(ifMatch), ch, agents.Certifications{Agents: identity.AutonomousAgents})
			return err
		})
		return p, err
	}
	off, limit := false, money.Minor(5000)
	capped := &limit
	routes := map[string]string{"erp-analytics": "claude-haiku-5"}
	allowed := []string{"claude-opus-5-5", "claude-haiku-5"}
	p, err := update(`"1"`, agents.PolicyChange{Enabled: &off, MonthlyCap: &capped, Routes: &routes, AllowedModels: &allowed})
	if err != nil {
		t.Fatal(err)
	}
	if p.Active() || p.MonthlyCap == nil || *p.MonthlyCap != 5000 || p.Routes["erp-analytics"] != "claude-haiku-5" || p.Version != 2 {
		t.Errorf("after the change: got %+v, want AI off, a 50.00 cap, analytics on claude-haiku-5, version 2", p)
	}
	if p.UpdatedBy == nil || *p.UpdatedBy != ten.Human.ID {
		t.Errorf("updated by: got %v, want %s", p.UpdatedBy, ten.Human.ID)
	}
	t.Run("withholding documents", func(t *testing.T) {
		withheld := false
		p, err := update("*", agents.PolicyChange{DocumentsToModels: &withheld})
		if err != nil || p.DocumentsToModels {
			t.Errorf("got documents_to_models %v, %v; want false", p.DocumentsToModels, err)
		}
	})
	t.Run("clearing the cap", func(t *testing.T) {
		var none *money.Minor
		p, err := update("*", agents.PolicyChange{MonthlyCap: &none})
		if err != nil || p.MonthlyCap != nil {
			t.Errorf("got %v, %v; want no cap", p.MonthlyCap, err)
		}
	})
	for _, c := range []struct {
		name, ifMatch string
		ch            agents.PolicyChange
		want          string
	}{
		{"a stale version", `"1"`, agents.PolicyChange{Enabled: &off}, "request.version_mismatch"},
		{"an alias the platform does not route", "*", agents.PolicyChange{Routes: &map[string]string{"erp-poetry": "claude-haiku-5"}}, "agents.invalid_policy/unknown_alias"},
		{"a route to an unpriced model", "*", agents.PolicyChange{Routes: &map[string]string{"erp-default": "claude-unpriced-1"}, AllowedModels: &[]string{}}, "agents.invalid_policy/unpriced"},
		{"a route to a model not allowed", "*", agents.PolicyChange{AllowedModels: &[]string{"claude-haiku-5"}}, "agents.invalid_policy/not_allowed"},
		{"a route that is no model id", "*", agents.PolicyChange{Routes: &map[string]string{"erp-default": "Claude Opus"}}, "agents.invalid_policy/invalid"},
		{"an allowed model that is no model id", "*", agents.PolicyChange{AllowedModels: &[]string{"opus!"}}, "agents.invalid_policy/invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := update(c.ifMatch, c.ch); codeOf(err) != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
	t.Run("the fields a change sets", func(t *testing.T) {
		on := true
		if got := fmt.Sprint(agents.PolicyChange{DocumentsToModels: &on, Enabled: &on}.Fields()); got != "[documents_to_models enabled]" {
			t.Errorf("got %s, want [documents_to_models enabled]", got)
		}
	})
}

// TestSuspendAI: an operator's suspension stops the tenant's AI whatever
// its administrators set, says why, and on resume leaves their setting as
// it was.
func TestSuspendAI(t *testing.T) {
	ten := pgtest.NewTenant(t)
	operator := uuid.Must(uuid.NewV7())
	var p agents.Policy
	run := func(fn func(ctx context.Context, tx pgx.Tx) (agents.Policy, error)) error {
		return inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) (err error) {
			p, err = fn(ctx, tx)
			return err
		})
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
		return agents.Suspend(ctx, tx, operator, etag.IfMatch("*"), "billing dispute")
	}); err != nil {
		t.Fatal(err)
	}
	if p.Active() {
		t.Errorf("suspended active: got true, want false")
	}
	if p.SuspendedReason == nil || *p.SuspendedReason != "billing dispute" {
		t.Errorf("reason: got %v, want billing dispute", p.SuspendedReason)
	}
	if !p.Enabled {
		t.Errorf("the tenant's setting: got off, want on, unchanged")
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
		return agents.Resume(ctx, tx, operator, etag.IfMatch("*"))
	}); err != nil {
		t.Fatal(err)
	}
	if !p.Active() {
		t.Errorf("resumed active: got false, want true")
	}
	if p.SuspendedReason != nil {
		t.Errorf("resumed reason: got %v, want none", *p.SuspendedReason)
	}
	long := strings.Repeat("é", 500) // 500 characters, 1000 bytes: the column counts characters
	if err := run(func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
		return agents.Suspend(ctx, tx, operator, etag.IfMatch("*"), long)
	}); err != nil {
		t.Errorf("a reason of 500 characters: got %v, want it accepted", err)
	}
	for name, reason := range map[string]string{"no reason": "", "a reason past 500 characters": strings.Repeat("é", 501)} {
		t.Run(name, func(t *testing.T) {
			err := run(func(ctx context.Context, tx pgx.Tx) (agents.Policy, error) {
				return agents.Suspend(ctx, tx, operator, etag.IfMatch("*"), reason)
			})
			if codeOf(err) != "agents.invalid_policy/invalid_length" {
				t.Errorf("got %v, want agents.invalid_policy/invalid_length", err)
			}
		})
	}
}

// TestPolicyEvent: a policy's event carries its settings, the cap as an
// amount, and never the suspension's reason.
func TestPolicyEvent(t *testing.T) {
	ten := pgtest.NewTenant(t)
	limit := money.Minor(5000)
	reason := "billing dispute"
	p := agents.Policy{Enabled: true, Suspended: true, SuspendedReason: &reason, MonthlyCap: &limit, Routes: map[string]string{}, AllowedModels: []string{}}
	var e map[string]any
	if err := inTenant(t, ten, func(ctx context.Context, tx pgx.Tx) error {
		ev, err := agents.PolicyEvent(ctx, tx, ten.ID, p, []string{"suspended"})
		e, _ = ev.Payload.(map[string]any)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(e["monthly_cap"]); got != "map[amount:50.00 currency:USD]" {
		t.Errorf("cap: got %s, want 50.00 USD", got)
	}
	if e["active"] != false {
		t.Errorf("active: got %v, want false", e["active"])
	}
	for k, v := range e {
		if v == reason {
			t.Errorf("payload %s carries the suspension's reason", k)
		}
	}
}

// TestPolicyDatabaseFailures: each statement of reading, changing,
// suspending and resuming the policy, of admitting an agent under it and
// of its event, failing in turn
// fails the operation with that error.
func TestPolicyDatabaseFailures(t *testing.T) {
	priced(t, "claude-opus-5-5")
	ten := pgtest.NewTenant(t)
	actor := uuid.Must(uuid.NewV7())
	off, limit := false, money.Minor(100)
	capped := &limit
	ops := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"read":  func(ctx context.Context, tx pgx.Tx) error { _, err := agents.ReadPolicy(ctx, tx); return err },
		"admit": agents.Admit,
		"update": func(ctx context.Context, tx pgx.Tx) error {
			_, err := agents.UpdatePolicy(ctx, tx, actor, etag.IfMatch("*"), agents.PolicyChange{Enabled: &off, MonthlyCap: &capped}, agents.Certifications{})
			return err
		},
		"suspend": func(ctx context.Context, tx pgx.Tx) error {
			_, err := agents.Suspend(ctx, tx, actor, etag.IfMatch("*"), "audit")
			return err
		},
		"resume": func(ctx context.Context, tx pgx.Tx) error {
			_, err := agents.Resume(ctx, tx, actor, etag.IfMatch("*"))
			return err
		},
		"event": func(ctx context.Context, tx pgx.Tx) error {
			_, err := agents.PolicyEvent(ctx, tx, ten.ID, agents.Policy{MonthlyCap: &limit}, []string{"monthly_cap"})
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			for n := 1; ; n++ {
				ctx := context.Background()
				tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
				if err != nil {
					t.Fatal(err)
				}
				ftx := &pgtest.FailingTx{Tx: tx, FailAt: n}
				err = op(ctx, ftx)
				_ = tx.Rollback(ctx) // the probe commits nothing
				if ftx.Calls() < n {
					if err != nil {
						t.Fatalf("no statement failed: got %v, want success", err)
					}
					return
				}
				if !errors.Is(err, pgtest.ErrInjected) {
					t.Fatalf("statement %d failed: got %v, want the injected error", n, err)
				}
			}
		})
	}
}

// TestKillSwitch: while a tenant's AI is off, switched off by its
// administrators or suspended by the operators, the gateway refuses every
// caller's call with 403 agents.ai_disabled, forwards nothing and holds
// nothing (ADR-0036).
func TestKillSwitch(t *testing.T) {
	priced(t, "claude-opus-5-5")
	for _, c := range []struct{ name, set string }{
		{"switched off by the tenant", "enabled = false"},
		{"suspended by the operators", "suspended = true, suspended_reason = 'billing dispute'"},
	} {
		for kind, as := range map[string]func(pgtest.Tenant) reqctx.Principal{
			"an agent": principal,
			"a service": func(ten pgtest.Tenant) reqctx.Principal {
				return reqctx.Principal{ID: ten.Service.ID, Kind: reqctx.KindService, TenantID: ten.ID}
			},
		} {
			t.Run(c.name+", "+kind, func(t *testing.T) {
				ten := pgtest.NewTenant(t)
				pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id) VALUES ($1)`, ten.ID)
				pgtest.Exec(t, `UPDATE agents.ai_policy SET `+c.set+`, version = version + 1 WHERE tenant_id = $1`, ten.ID)
				p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)}
				rec := post(t, gateway(t, p), as(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "agents.ai_disabled") {
					t.Errorf("got %d %s, want 403 agents.ai_disabled", rec.Code, rec.Body)
				}
				if p.sent != nil {
					t.Error("the call was forwarded")
				}
				if n := count(t, ten, `SELECT count(*) FROM agents.model_reservation WHERE tenant_id = $1`); n != 0 {
					t.Errorf("reservations: got %d, want 0", n)
				}
			})
		}
	}
}

// TestTenantRouting: the tenant's policy routes its aliases over the
// platform's, and the gateway forwards only to a model the policy allows.
func TestTenantRouting(t *testing.T) {
	priced(t, "claude-opus-5-5")
	priced(t, "claude-haiku-5")
	t.Run("the tenant's route", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id, routes) VALUES ($1, '{"erp-default": "claude-haiku-5"}')`, ten.ID)
		p := &provider{status: 200, body: message("claude-haiku-5", "end_turn", 10, 1, 0, 0)}
		rec := post(t, gateway(t, p), principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d %s, want 200", rec.Code, rec.Body)
		}
		if !strings.Contains(string(p.sent), `"model":"claude-haiku-5"`) {
			t.Errorf("forwarded: got %s, want the tenant's model claude-haiku-5", p.sent)
		}
		if got := rec.Header().Get("Erp-Model"); got != "claude-haiku-5" {
			t.Errorf("Erp-Model: got %q, want claude-haiku-5", got)
		}
	})
	t.Run("a model the policy does not allow", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id, allowed_models) VALUES ($1, '{claude-haiku-5}')`, ten.ID)
		p := &provider{status: 200, body: message("claude-opus-5-5", "end_turn", 10, 1, 0, 0)}
		rec := post(t, gateway(t, p), principal(ten), `{"model": "erp-default", "max_tokens": 8, "messages": []}`, nil)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "agents.model_not_allowed") {
			t.Errorf("got %d %s, want 422 agents.model_not_allowed", rec.Code, rec.Body)
		}
		if p.sent != nil {
			t.Error("the call was forwarded")
		}
	})
}
