package mrp_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// approving is valued with company 2000's S&OP plans approved in two
// steps: one sop_lead, then one controller.
func approving(c *config.Config) {
	valued(c)
	c.Company("2000").SopApproval = config.SopApproval{Steps: []config.ApprovalStep{{Role: "sop_lead", Approvals: 1}, {Role: "controller", Approvals: 1}}}
}

// request reads the approval request and its ETag.
func (e env) request(t *testing.T, id string) (map[string]any, string) {
	t.Helper()
	return e.Must(200, "GET", "/v1/approval-requests/"+id, "", "")
}

// submitted submits a reviewed plan version for approval and returns it
// with its ETag and request.
func (e env) submitted(t *testing.T) (p map[string]any, tag, request string) {
	t.Helper()
	e.plan(t, e.bracket, "250")
	p, tag = e.review(t, 1)
	p, tag = e.Must(200, "POST", sops+"/"+p["id"].(string)+":submit", "", tag)
	return p, tag, apitest.String(t, p, "approval_request_id")
}

// TestSopApproval (S2n2; ADR-0084 As built S2n2): where company 2000's
// plans are approved, the planner's fix is refused and her submission
// opens a request of two steps; submitted, the version's consensus, its
// fix and a second submission are refused; rejected it is a draft again,
// changed and submitted anew; withdrawn a draft again; approved by a
// distinct person at each step, it is fixed by the last step's approver,
// naming the approved request. Each step is audited.
func TestSopApproval(t *testing.T) {
	e := setupWith(t, approving)
	e.plan(t, e.bracket, "250")
	p, tag := e.review(t, 1)
	id := p["id"].(string)
	m1, _, _, _ := months(t, p)
	key := "BRACKETS/EA/" + m1
	if status, got, _ := e.Do("POST", sops+"/"+id+":fix", "", tag); status != 409 || got["code"] != "mrp.sop_approval_required" {
		t.Fatalf("a fix: got %d %v, want 409 mrp.sop_approval_required", status, got["code"])
	}
	p, tag = e.Must(200, "POST", sops+"/"+id+":submit", "", tag)
	first := apitest.String(t, p, "approval_request_id")
	if got := fmt.Sprintf("%v %v", p["status"], p["fixed_by"]); got != "submitted <nil>" {
		t.Errorf("submitted: got %s, want submitted <nil>", got)
	}
	r, _ := e.request(t, first)
	if got := fmt.Sprintf("%v %v %v %d", r["object_type"], r["object_id"], r["object_resource"], len(rows(t, r, "steps"))); got != "sop_plan "+id+" "+sops+"/"+id+" 2" {
		t.Errorf("the request: got %s, want the plan's, of two steps", got)
	}
	for name, do := range map[string]func() (int, map[string]any, string){
		"its consensus": func() (int, map[string]any, string) {
			return e.Do("PATCH", sops+"/"+id, fmt.Sprintf(`{"consensus": {%q: "1"}}`, key), tag)
		},
		"a fix":       func() (int, map[string]any, string) { return e.Do("POST", sops+"/"+id+":fix", "", tag) },
		"resubmitted": func() (int, map[string]any, string) { return e.Do("POST", sops+"/"+id+":submit", "", tag) },
	} {
		t.Run(name, func(t *testing.T) {
			if status, got, _ := do(); status != 409 || got["code"] != "mrp.sop_plan_submitted" {
				t.Errorf("got %d %v, want 409 mrp.sop_plan_submitted", status, got["code"])
			}
		})
	}
	_, rtag := e.request(t, first)
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+first+":reject", `{"comment": "Too low for the season."}`, rtag)
	p, tag = e.Must(200, "GET", sops+"/"+id, "", "")
	if got := fmt.Sprintf("%v %v", p["status"], p["approval_request_id"]); got != "draft "+first {
		t.Errorf("rejected: got %s, want a draft naming the rejected request", got)
	}
	_, tag = e.Must(200, "PATCH", sops+"/"+id, fmt.Sprintf(`{"consensus": {%q: "300"}}`, key), tag)
	p, tag = e.Must(200, "POST", sops+"/"+id+":submit", "", tag)
	second := apitest.String(t, p, "approval_request_id")
	_, rtag = e.request(t, second)
	e.Must(200, "POST", "/v1/approval-requests/"+second+":cancel", `{"comment": "One more change."}`, rtag)
	p, tag = e.Must(200, "GET", sops+"/"+id, "", "")
	if p["status"] != "draft" {
		t.Errorf("withdrawn: got %v, want draft", p["status"])
	}
	p, _ = e.Must(200, "POST", sops+"/"+id+":submit", "", tag)
	third := apitest.String(t, p, "approval_request_id")
	approve := func(a pgtest.Actor) (int, map[string]any) {
		_, rtag := e.request(t, third)
		status, got, _ := e.As(a).Do("POST", "/v1/approval-requests/"+third+":approve", `{"comment": "Agreed"}`, rtag)
		return status, got
	}
	if status, _ := approve(e.Approver); status != 200 {
		t.Fatalf("the first step: got %d, want 200", status)
	}
	if p, _ = e.Must(200, "GET", sops+"/"+id, "", ""); p["status"] != "submitted" {
		t.Errorf("after the first step: got %v, want submitted", p["status"])
	}
	if status, got := approve(e.Approver); status != 409 || got["code"] != "workflow.already_approved" {
		t.Errorf("the second step by the first approver: got %d %v, want 409 workflow.already_approved", status, got["code"])
	}
	if status, _ := approve(e.Approver2); status != 200 {
		t.Fatalf("the second step: got %d, want 200", status)
	}
	p, _ = e.Must(200, "GET", sops+"/"+id, "", "")
	if got, want := fmt.Sprintf("%v %v %v %v", p["status"], p["fixed_by"], p["approval_request_id"], strings.Fields(sopLine(t, p, key))[6]), "fixed "+e.Approver2.ID.String()+" "+third+" 300"; got != want {
		t.Errorf("approved: got %s, want %s", got, want)
	}
	var actions string
	pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`, []any{e.Tenant.ID, uuid.MustParse(id)},
		&actions)
	if want := "mrp.sop_plan.create mrp.sop_plan.submit mrp.sop_plan.reject mrp.sop_plan.consensus mrp.sop_plan.submit mrp.sop_plan.withdraw " +
		"mrp.sop_plan.submit mrp.sop_plan.fix"; actions != want {
		t.Errorf("audited: got %s, want %s", actions, want)
	}
}

// TestSopSubmitWithoutApproval (S2n2): a company that names no steps, the
// default, fixes a submitted version at once, by the planner, naming no
// request, as a fix does.
func TestSopSubmitWithoutApproval(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	p, tag := e.review(t, 1)
	p, _ = e.Must(200, "POST", sops+"/"+p["id"].(string)+":submit", "", tag)
	if got, want := fmt.Sprintf("%v %v %v", p["status"], p["fixed_by"], p["approval_request_id"]), "fixed "+e.Tenant.Human.ID.String()+" <nil>"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestSopApprovalRefusals (S2n2): a step naming a role that is no longer
// active refuses the submission (422 workflow.unknown_role) and leaves the
// version a draft; an engine calling back about a version that is not
// waiting for that request fails as a fault (500), not a refusal.
func TestSopApprovalRefusals(t *testing.T) {
	e := setupWith(t, approving)
	p, _, request := e.submitted(t)
	id := uuid.MustParse(p["id"].(string))
	for name, c := range map[string]struct {
		call func(ctx context.Context) error
		code string // "": a fault
	}{
		"another request": {func(ctx context.Context) error { return mrp.SopApprovals{}.Approved(ctx, id, uuid.New()) }, ""},
		"no such version": {func(ctx context.Context) error {
			return mrp.SopApprovals{}.Rejected(ctx, uuid.New(), uuid.MustParse(request), "no")
		}, "request.not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, e.Tenant, e.Approver, c.call); err == nil || codeOf(err) != c.code {
				t.Errorf("got %v, want %q", err, c.code)
			}
		})
	}
	_, rtag := e.request(t, request)
	e.Must(200, "POST", "/v1/approval-requests/"+request+":cancel", `{"comment": "Retry"}`, rtag)
	if err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		return mrp.SopApprovals{}.Cancelled(ctx, id, uuid.MustParse(request), "again")
	}); err == nil {
		t.Error("a draft called back: got nil error, want a fault")
	}
	_, tag := e.Must(200, "GET", sops+"/"+id.String(), "", "")
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND code = 'sop_lead'`, e.Tenant.ID)
	if status, got, _ := e.Do("POST", sops+"/"+id.String()+":submit", "", tag); status != 422 || got["code"] != "workflow.unknown_role" {
		t.Errorf("an inactive role: got %d %v, want 422 workflow.unknown_role", status, got["code"])
	}
	if p, _ := e.Must(200, "GET", sops+"/"+id.String(), "", ""); p["status"] != "draft" {
		t.Errorf("after the refusal: got %v, want draft", p["status"])
	}
	paths, err := mrp.SopApprovals{}.Resources(context.Background(), []uuid.UUID{id})
	if got, want := paths[id], sops+"/"+id.String(); err != nil || got != want {
		t.Errorf("its resource: got %q, %v, want %q", got, err, want)
	}
}

// TestSopApprovalDatabaseFailures calls submitting where approval is
// required and each callback outside a unit of work and with each of its
// database statements failing in turn (a fix's are TestSopDatabaseFailures').
func TestSopApprovalDatabaseFailures(t *testing.T) {
	e := setupWith(t, approving)
	h := endpoints(t)
	p, _, request := e.submitted(t)
	id, rid := uuid.MustParse(p["id"].(string)), uuid.MustParse(request)
	draft, dtag := e.review(t, 1)
	did := uuid.MustParse(draft["id"].(string))
	approver := map[string]func(ctx context.Context) error{
		"approved": func(ctx context.Context) error { return mrp.SopApprovals{}.Approved(ctx, id, rid) },
		"rejected": func(ctx context.Context) error { return mrp.SopApprovals{}.Rejected(ctx, id, rid, "no") },
	}
	planner := map[string]func(ctx context.Context) error{
		"submit": func(ctx context.Context) error {
			_, err := h.SubmitSopPlan(ctx, gen.SubmitSopPlanParams{ID: did, IfMatch: dtag})
			return err
		},
	}
	for who, calls := range map[string]map[string]func(ctx context.Context) error{"approver": approver, "planner": planner} {
		a := e.Approver
		if who == "planner" {
			a = e.Tenant.Human
		}
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				if err := call(context.Background()); err == nil {
					t.Error("outside a unit of work: got nil error, want an error")
				}
				apitest.EveryFailure(t, e.Tenant, a, name, call)
			})
		}
	}
}
