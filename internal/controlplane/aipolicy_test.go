package controlplane_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestAIKillSwitchThroughTheControlPlane: operators suspend a tenant's AI
// with a reason, whatever the tenant set, and resume it, leaving the
// tenant's own setting; each change is audited and published without the
// reason (ADR-0036).
func TestAIKillSwitchThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	policy := tenants + "/" + ten.ID.String() + "/ai-policy"
	got, h := p.must(200, "ops|alice", "GET", policy, "")
	if got["active"] != true {
		t.Errorf("default active: got %v, want true", got["active"])
	}
	if h.Get("ETag") != `"1"` {
		t.Errorf("default ETag: got %s, want \"1\"", h.Get("ETag"))
	}
	reason := "Unpaid invoices since August"
	got, h = p.must(200, "ops|alice", "POST", policy+":suspend", `{"reason": "`+reason+`"}`, "If-Match", `"1"`)
	if got["active"] != false {
		t.Errorf("suspended active: got %v, want false", got["active"])
	}
	if got["suspended_reason"] != reason {
		t.Errorf("reason: got %v, want %s", got["suspended_reason"], reason)
	}
	if _, op := auditAction(t, ten.ID, "agents.ai_policy.suspend"); op != "ops|alice" {
		t.Errorf("audit: got operator %q, want ops|alice", op)
	}
	if d := fmt.Sprint(auditDetail(t, ten.ID, "agents.ai_policy.suspend")); strings.Contains(d, reason) {
		t.Errorf("audit detail carries the reason: %s", d)
	}
	pgtest.Exec(t, `UPDATE agents.ai_policy SET enabled = false, version = version + 1 WHERE tenant_id = $1`, ten.ID)
	p.problem(412, "request.version_mismatch", "ops|alice", "POST", policy+":resume", "", "If-Match", h.Get("ETag"))
	got, _ = p.must(200, "ops|alice", "POST", policy+":resume", "", "If-Match", "*")
	if got["suspended"] != false {
		t.Errorf("resumed suspended: got %v, want false", got["suspended"])
	}
	if got["enabled"] != false {
		t.Errorf("resumed enabled: got %v, want the tenant's false", got["enabled"])
	}
	events := apitest.Published(t, ten, outbox.Event{Context: "agents", Name: "ai_policy_changed"})
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	for _, e := range events {
		if strings.Contains(fmt.Sprint(e), reason) {
			t.Errorf("event carries the reason: %v", e)
		}
	}
}

func TestAIKillSwitchRequestsRefused(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	policy := tenants + "/" + ten.ID.String() + "/ai-policy"
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		code                     string
		extra                    []string
	}{
		{"an unknown tenant", "GET", tenants + "/" + uuid.NewString() + "/ai-policy", "", 404, "request.not_found", nil},
		{"no If-Match", "POST", policy + ":suspend", `{"reason": "r"}`, 428, "request.precondition_required", nil},
		{"no reason", "POST", policy + ":suspend", `{"reason": ""}`, 422, "agents.invalid_policy", []string{"If-Match", "*"}},
		{"an unknown field", "POST", policy + ":suspend", `{"reason": "r", "until": "never"}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a body too large", "POST", policy + ":suspend", `{"reason": "` + strings.Repeat("r", 5000) + `"}`, 413, "request.body_too_large", []string{"If-Match", "*"}},
		{"a write to the policy", "PATCH", policy, `{}`, 405, "request.method_not_allowed", nil},
		{"a read of a suspension", "GET", policy + ":suspend", "", 405, "request.method_not_allowed", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.in(t).problem(c.status, c.code, "ops|alice", c.method, c.path, c.body, c.extra...)
		})
	}
}

func TestAIKillSwitchDatabaseFailures(t *testing.T) {
	p := newPlane(t)
	s := p.s
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `INSERT INTO agents.ai_policy (tenant_id, monthly_cap, cap_currency) VALUES ($1, 100, 'USD')`, ten.ID) // the event reads the cap's currency
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: p.is.URL, Subject: "ops|alice"})
	for name, fn := range map[string]func(ctx context.Context) error{
		"read":    func(ctx context.Context) error { _, err := s.AIPolicy(ctx, ten.ID); return err },
		"suspend": func(ctx context.Context) error { _, err := s.SuspendAI(ctx, op, ten.ID, "*", "r"); return err },
		"resume":  func(ctx context.Context) error { _, err := s.ResumeAI(ctx, op, ten.ID, "*"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			everyFailure(t, s, name, fn)
		})
	}
}
