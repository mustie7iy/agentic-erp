package configuration_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// failingSampler cannot read its rows.
type failingSampler struct{}

func (failingSampler) Sample(context.Context, time.Time, int) ([]rules.Sample, error) {
	return nil, errors.New("boom")
}

// partnerRule is a business partner rule with its two cases: a partner
// of the country and roles of passes passes, one of refused's does not.
func partnerRule(expression string, passes, refused map[string]any) rules.Rule {
	row := func(m map[string]any) map[string]any {
		r := map[string]any{"kind": "organization", "name": "N", "country_code": "DE", "roles": []any{}}
		maps.Copy(r, m)
		return r
	}
	return rules.Rule{Expression: expression, Message: "Refused.", Cases: []rules.Case{{Row: row(passes), Valid: true}, {Row: row(refused), Valid: false}}}
}

// noUS refuses partners in the United States, as the fixture's suppliers are.
func noUS() rules.Rule {
	return partnerRule(`row.country_code != "US"`, nil, map[string]any{"country_code": "US"})
}

// rulesChange proposes a business partner rule on version 1, submits it
// (tier 3) and asks for its simulation; it returns the change request's
// path and the operation's id.
func (s sim) rulesChange(t *testing.T, name string, r rules.Rule) (string, uuid.UUID) {
	t.Helper()
	doc := fixture.Document(t)
	doc.Rules = map[string]map[string]rules.Rule{"business_partner": {name: r}}
	return s.tier3Change(t, 1, doc)
}

// tier3Change proposes a document on a version, submits it, wants it
// tier 3, and asks for its simulation; it returns the change request's
// path and the operation's id.
func (s sim) tier3Change(t *testing.T, base int, doc *config.Config) (string, uuid.UUID) {
	t.Helper()
	cr, _ := s.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Rules", "rationale": "Policy", "base_version": %d, "proposed": %s}`, base, jsonOf(t, doc)), "")
	path := "/v1/change-requests/" + apitest.String(t, cr, "id")
	got, tag := s.Must(200, "POST", path+":submit", "", `"1"`)
	if got["status"] != "awaiting_evidence" || got["tier"] != float64(3) {
		t.Fatalf("submitted: got status %v tier %v, want awaiting_evidence and 3", got["status"], got["tier"])
	}
	op, _ := s.Must(202, "POST", path+":simulate", "", tag)
	return path, uuid.MustParse(apitest.String(t, op, "id"))
}

// replayOf is the business partner replay of a change request's latest
// simulation evidence.
func replayOf(t *testing.T, got map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	e := evidenceOf(t, got, "simulation")
	replays := apitest.List(t, apitest.Object(t, e, "detail"), "rules")
	if len(replays) != 1 {
		t.Fatalf("rule replays: got %v, want one", replays)
	}
	return e, replays[0].(map[string]any)
}

// TestRulesSimulatedBeforeActivation (T58h): a rules change is simulated on
// the recent rows of its entity: each rule's newly refused rows are
// evidence for approvers, and the change moves on to approval and
// activates. A proposed rule that cannot be evaluated on a row fails the
// simulation, so the change cannot reach approval. A job runs once.
func TestRulesSimulatedBeforeActivation(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "platform.operation.cancel")...)}
	s.suite(t)
	w := worker(t, &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "0 invoices replayed."}})
	ctx := context.Background()

	t.Run("cannot be evaluated", func(t *testing.T) {
		s := s.in(t)
		path, op := s.rulesChange(t, "second_role", partnerRule(`row.roles[1] != "x"`, map[string]any{"roles": []any{"a", "b"}}, map[string]any{"roles": []any{"a", "x"}}))
		if did, err := w.Simulate(ctx, s.Tenant.ID, op); !did || err != nil {
			t.Fatalf("simulate: got %t %v, want one run", did, err)
		}
		got := s.status(t, path)
		if got["status"] != "awaiting_evidence" {
			t.Errorf("status: got %v, want awaiting_evidence", got["status"])
		}
		e, replay := replayOf(t, got)
		if e["passed"] != false || !strings.Contains(e["summary"].(string), "cannot activate") {
			t.Errorf("evidence: got passed %v, summary %q, want a failure saying the rules cannot activate", e["passed"], e["summary"])
		}
		rule := apitest.List(t, replay, "rules")[0].(map[string]any)
		if rule["rule"] != "second_role" || rule["unevaluable"] != float64(2) {
			t.Errorf("rule: got %v, want second_role unevaluable on both partners", rule)
		}
	})

	t.Run("a sampler that fails, or none", func(t *testing.T) {
		s := s.in(t)
		for name, samplers := range map[string]map[string]rules.Sampler{
			"fails": {"business_partner": failingSampler{}},
			"none":  nil,
		} {
			t.Run(name, func(t *testing.T) {
				_, op := s.in(t).rulesChange(t, "us_"+name, noUS())
				failing := *w
				failing.Dependencies.RuleSamplers = samplers
				if _, err := failing.Simulate(ctx, s.Tenant.ID, op); err == nil {
					t.Fatal("simulate: got no error, want one")
				}
				if did, err := w.Simulate(ctx, s.Tenant.ID, op); !did || err != nil {
					t.Errorf("retry: got %t %v, want the job still queued and run", did, err)
				}
			})
		}
	})

	path, op := s.rulesChange(t, "no_us", noUS())
	for run, want := range []bool{true, false} { // the second run finds the job done
		if did, err := w.Simulate(ctx, s.Tenant.ID, op); did != want || err != nil {
			t.Fatalf("run %d: got %t %v, want %t", run+1, did, err, want)
		}
	}
	got := s.status(t, path)
	if got["status"] != "pending_approval" {
		t.Fatalf("status: got %v, want pending_approval", got["status"])
	}
	if n := len(apitest.List(t, got, "evidence")); n != 2 {
		t.Errorf("evidence: got %d, want the simulation and the regression once", n)
	}
	e, replay := replayOf(t, got)
	if !strings.Contains(e["summary"].(string), "Rules on business_partner") || apitest.Object(t, e, "detail")["proposal_sha256"] == "" {
		t.Errorf("evidence: got %v, want the rules in the summary and the proposal's hash", e)
	}
	rule := apitest.List(t, replay, "rules")[0].(map[string]any)
	refused := apitest.List(t, rule, "refused_rows")
	if rule["rule"] != "no_us" || rule["newly_refused"] != float64(2) || len(refused) != 2 {
		t.Fatalf("rule: got %v, want no_us refusing both partners", rule)
	}
	if ref := refused[0].(map[string]any)["reference"]; ref != "BP90000001" && ref != "BP90000002" {
		t.Errorf("refused row: got %v, want a partner's number", ref)
	}
	s.activateSimulated(t, path)

	// A change of only the custom fields the active rules read is a rules
	// change too: tier 3, simulated, and activated on that evidence only.
	doc := fixture.Document(t)
	doc.Rules = map[string]map[string]rules.Rule{"business_partner": {"no_us": noUS()}}
	schema := doc.Extensions["business_partner"]
	schema.Version++
	schema.Properties = maps.Clone(schema.Properties)
	schema.Properties["region"] = extensions.Property{Type: "string", MaxLength: 10, PII: "none"}
	doc.Extensions["business_partner"] = schema
	s.suite(t) // on version 2
	path, op = s.tier3Change(t, 2, doc)
	if did, err := w.Simulate(ctx, s.Tenant.ID, op); !did || err != nil {
		t.Fatalf("simulate the schema change: got %t %v, want one run", did, err)
	}
	s.activateSimulated(t, path)
}

// activateSimulated approves a simulated change and activates it, after
// showing that evidence of another proposal does not vouch for it.
func (s sim) activateSimulated(t *testing.T, path string) {
	t.Helper()
	got := s.status(t, path)
	if got["status"] != "pending_approval" {
		t.Fatalf("status: got %v, want pending_approval (evidence %v)", got["status"], got["evidence"])
	}
	hash := apitest.Object(t, evidenceOf(t, got, "simulation"), "detail")["proposal_sha256"]
	s.Approve(t, strings.TrimPrefix(path, "/v1/change-requests/"))
	_, tag := s.Must(200, "GET", path, "", "")
	setHash := func(h any) {
		pgtest.Unguarded(t, `UPDATE config.evidence SET detail = jsonb_set(detail, '{proposal_sha256}', to_jsonb($2::text))
			WHERE tenant_id = $1 AND kind = 'simulation' AND change_request_id = $3`, s.Tenant.ID, h, strings.TrimPrefix(path, "/v1/change-requests/"))
	}
	setHash("another")
	s.Problem(409, "config.rules_not_simulated", "POST", path+":activate", "", tag)
	setHash(hash)
	if active, _ := s.Must(200, "POST", path+":activate", "", tag); active["status"] != "active" {
		t.Errorf("activated: got %v, want active", active["status"])
	}
}
