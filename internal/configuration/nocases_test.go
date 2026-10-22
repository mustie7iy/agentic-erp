package configuration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestRegressionEvidenceWithoutCases: a fresh tenant, with no invoices to
// capture, records a suite of no cases only when it asks for one; the
// regression evidence replaying it passes, says that nothing was
// replayed, and every approver must acknowledge that in their comment
// before the tier 3 change is approved (T72l).
func TestRegressionEvidenceWithoutCases(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read", "config.regression.manage")...)}
	s.Problem(422, "config.no_cases", "POST", "/v1/regression-suites", `{"name": "Fresh"}`, "")
	suite, _ := s.Must(201, "POST", "/v1/regression-suites", `{"name": "Fresh", "allow_empty": true}`, "")
	t.Run("the suite", func(t *testing.T) {
		if suite["case_count"] != float64(0) {
			t.Errorf("case count: got %v, want 0", suite["case_count"])
		}
		if n := len(apitest.List(t, suite, "cases")); n != 0 {
			t.Errorf("cases: got %d, want 0", n)
		}
		if n := len(apitest.List(t, suite, "strata")); n != 0 {
			t.Errorf("strata: got %d, want 0", n)
		}
	})
	cr, _ := s.tier3(t, "9")
	path := "/v1/change-requests/" + apitest.String(t, cr, "id")
	w := &configuration.SimulationWorker{Dependencies: fixture.Deps(), Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker"}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.status(t, path)
	if got["status"] != "pending_approval" {
		t.Fatalf("status: got %v, want pending_approval (evidence %v)", got["status"], got["evidence"])
	}
	t.Run("the evidence says nothing was replayed", func(t *testing.T) {
		regression := evidenceOf(t, got, "regression")
		if regression["passed"] != true {
			t.Errorf("regression evidence passed: got %v, want true", regression["passed"])
		}
		if summary := apitest.String(t, regression, "summary"); !strings.Contains(summary, "holds no cases") {
			t.Errorf("regression evidence summary: got %q, want it to say the suite holds no cases", summary)
		}
		if none := apitest.Object(t, regression, "detail")["no_cases"]; none != true {
			t.Errorf("regression evidence no_cases: got %v, want true", none)
		}
	})
	r := s.Pending(t, apitest.String(t, cr, "id"))
	approve := "/v1/approval-requests/" + apitest.String(t, r, "id") + ":approve"
	tag := fmt.Sprintf(`"%v"`, r["version"])
	t.Run("an approval without a comment", func(t *testing.T) {
		s.As(s.Approver).Problem(422, "workflow.acknowledgement_required", "POST", approve, `{}`, tag)
		s.As(s.Approver).Problem(422, "workflow.acknowledgement_required", "POST", approve, `{"comment": "  "}`, tag)
	})
	t.Run("approvals that acknowledge it", func(t *testing.T) {
		s.Approve(t, apitest.String(t, cr, "id"))
		if got := s.status(t, path)["status"]; got != "approved" {
			t.Errorf("status after the approvals: got %v, want approved", got)
		}
	})
	t.Run("what the approvers acknowledge", func(t *testing.T) {
		id := uuid.MustParse(apitest.String(t, cr, "id"))
		ack := func(ctx context.Context) error {
			_, err := configuration.Approvals{}.Acknowledgement(ctx, id)
			return err
		}
		if ack(context.Background()) == nil {
			t.Error("outside a unit of work: got nil error, want an error")
		}
		apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "acknowledgement", ack)
		for name, c := range map[string]struct {
			id   uuid.UUID
			want bool
		}{"no regression evidence": {uuid.New(), false}, "no cases replayed": {id, true}} {
			t.Run(name, func(t *testing.T) {
				var what string
				if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) (err error) {
					what, err = configuration.Approvals{}.Acknowledgement(ctx, c.id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if got := what != ""; got != c.want {
					t.Errorf("something to acknowledge: got %q, want it %v", what, c.want)
				}
			})
		}
		// Newer evidence that replayed cases needs no acknowledgement.
		pgtest.Exec(t, `INSERT INTO config.evidence (tenant_id, change_request_id, kind, passed, summary, detail, recorded_by, recorded_at)
			VALUES ($1, $2, 'regression', true, 'Suite RS-0002 replayed under the proposal.', '{"number": "RS-0002", "cases_by_company": []}', $3, now() + interval '1 second')`,
			s.Tenant.ID, id, s.Tenant.Human.ID)
		var what string
		if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) (err error) {
			what, err = configuration.Approvals{}.Acknowledgement(ctx, id)
			return err
		}); err != nil || what != "" {
			t.Errorf("after evidence with cases: got %q, %v, want nothing to acknowledge", what, err)
		}
	})
}
