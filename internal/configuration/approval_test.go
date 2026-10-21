package configuration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// TestApprovalDecisions follows tier 2 changes through their approval
// requests: approved and activated, rejected, withdrawn by the author.
func TestApprovalDecisions(t *testing.T) {
	f := fixture.New(t, perms...)
	propose := func(days int) map[string]any {
		doc := fixture.Document(t)
		doc.PaymentTerms[0].NetDays += days
		b, _ := json.Marshal(doc)
		cr, _ := f.Must(201, "POST", "/v1/change-requests", `{"title": "Terms", "base_version": 1, "proposed": `+string(b)+`}`, "")
		got, _ := f.Must(200, "POST", "/v1/change-requests/"+cr["id"].(string)+":submit", "", `"1"`)
		if got["status"] != "pending_approval" {
			t.Fatalf("submitted: got status %v, want pending_approval", got["status"])
		}
		if got["workflow_request_id"] == nil {
			t.Fatalf("submitted: got no workflow_request_id, want one: %v", got)
		}
		return got
	}
	approved := propose(1)
	r := f.Pending(t, approved["id"].(string))
	if r["object_type"] != "config_change" {
		t.Errorf("approval request object_type: got %v, want config_change", r["object_type"])
	}
	if r["id"] != approved["workflow_request_id"] {
		t.Errorf("approval request id: got %v, want %v", r["id"], approved["workflow_request_id"])
	}
	if n := len(r["steps"].([]any)); n != 1 {
		t.Errorf("approval request steps: got %d, want 1", n)
	}
	f.Approve(t, approved["id"].(string))
	got, tag := f.Must(200, "GET", "/v1/change-requests/"+approved["id"].(string), "", "")
	if got["status"] != "approved" {
		t.Fatalf("after approval: %v", got)
	}
	if active, _ := f.Must(200, "POST", "/v1/change-requests/"+approved["id"].(string)+":activate", "", tag); active["status"] != "active" {
		t.Fatalf("activated: %v", active)
	}

	rejected := propose(2)
	rr := f.Pending(t, rejected["id"].(string))
	f.As(f.Approver).Must(200, "POST", "/v1/approval-requests/"+rr["id"].(string)+":reject", `{"comment": "Not now"}`, `"1"`)
	if got, _ := f.Must(200, "GET", "/v1/change-requests/"+rejected["id"].(string), "", ""); got["status"] != "rejected" {
		t.Fatalf("after rejection: %v", got)
	}

	withdrawn := propose(3)
	wr := f.Pending(t, withdrawn["id"].(string))
	f.Must(200, "POST", "/v1/approval-requests/"+wr["id"].(string)+":cancel", `{"comment": "Wrong terms"}`, `"1"`)
	if got, _ := f.Must(200, "GET", "/v1/change-requests/"+withdrawn["id"].(string), "", ""); got["status"] != "withdrawn" {
		t.Fatalf("after cancellation: %v", got)
	}
	for _, name := range []string{"change_request_approved", "change_request_rejected", "change_request_withdrawn"} {
		t.Run(name, func(t *testing.T) {
			if len(events(t, f.Tenant, name)) == 0 {
				t.Error("got no event, want one")
			}
		})
	}

	// A decision for a change request that is no longer pending is refused.
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		return configuration.Approvals{}.Approved(ctx, uuid.MustParse(rejected["id"].(string)), uuid.New())
	})
	if err == nil {
		t.Fatal("approved a rejected change request")
	}
	for name, call := range map[string]func(context.Context) error{
		"approved": func(ctx context.Context) error {
			return configuration.Approvals{}.Approved(ctx, uuid.New(), uuid.New())
		},
		"rejected": func(ctx context.Context) error {
			return configuration.Approvals{}.Rejected(ctx, uuid.New(), uuid.New(), "c")
		},
		"cancelled": func(ctx context.Context) error {
			return configuration.Approvals{}.Cancelled(ctx, uuid.New(), uuid.New(), "c")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	pendingCR := propose(4)
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "approved", func(ctx context.Context) error {
		return configuration.Approvals{}.Approved(ctx, uuid.MustParse(pendingCR["id"].(string)), uuid.New())
	})
}

// noApproval is a workflow that needs no approval for anything.
type noApproval struct{ approvals.Approvals }

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func TestChangeWithoutApprovalSteps(t *testing.T) {
	f := fixture.New(t, perms...)
	doc := fixture.Document(t)
	doc.PaymentTerms[0].NetDays += 1
	b, _ := json.Marshal(doc)
	cr, _ := f.Must(201, "POST", "/v1/change-requests", `{"title": "Terms", "base_version": 1, "proposed": `+string(b)+`}`, "")
	deps := fixture.Deps()
	deps.Approvals = noApproval{}
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		got, err := configuration.SubmitChangeRequest(ctx, deps, uuid.MustParse(cr["id"].(string)), func(int32) error { return nil })
		if err != nil {
			return err
		}
		if got.Status != configuration.Approved {
			t.Errorf("submitted status: got %s, want %s", got.Status, configuration.Approved)
		}
		if got.WorkflowRequestID != nil {
			t.Errorf("submitted workflow request: got %v, want none", got.WorkflowRequestID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSettings(t *testing.T) {
	f := fixture.New(t, perms...)
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		doc, n, err := configuration.Settings{}.Document(ctx)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("document version: got %d, want 1", n)
		}
		if len(doc.Companies) != 2 {
			t.Errorf("document companies: got %d, want 2", len(doc.Companies))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bare := setup(t, true)
	if err := apitest.InRequest(t, bare.ten, bare.ten.Human, func(ctx context.Context) error {
		_, _, err := configuration.Settings{}.Document(ctx)
		return err
	}); err == nil {
		t.Error("document of a tenant without configuration")
	}
	if _, _, err := (configuration.Settings{}).Document(context.Background()); err == nil {
		t.Error("outside a unit of work")
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "settings", func(ctx context.Context) error {
		_, _, err := configuration.Settings{}.Document(ctx)
		return err
	})
	// The custom field schemas: the entity's in the active document, none
	// for an entity it does not declare or a tenant without configuration.
	schema := func(ten pgtest.Tenant, a pgtest.Actor, entity string) (s *extensions.Schema) {
		t.Helper()
		if err := apitest.InRequest(t, ten, a, func(ctx context.Context) (err error) {
			s, err = configuration.Settings{}.Schema(ctx, entity)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := schema(f.Tenant, f.Tenant.Human, "purchase_order"); s == nil || s.Version != 1 || len(s.Properties) != 2 {
		t.Errorf("purchase order schema: got %+v, want version 1 with 2 fields", s)
	}
	if s := schema(f.Tenant, f.Tenant.Human, "legal_entity"); s != nil {
		t.Errorf("undeclared entity: got %+v, want none", s)
	}
	if s := schema(bare.ten, bare.ten.Human, "purchase_order"); s != nil {
		t.Errorf("tenant without configuration: got %+v, want none", s)
	}
	if _, err := (configuration.Settings{}).Schema(context.Background(), "purchase_order"); err == nil {
		t.Error("schema outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "schema", func(ctx context.Context) error {
		_, err := configuration.Settings{}.Schema(ctx, "purchase_order")
		return err
	})
	// The rules: the entity's in the active document with its schema and
	// the version that names them, none without configuration.
	activeRules := func(ten pgtest.Tenant, a pgtest.Actor) (r rules.Active) {
		t.Helper()
		if err := apitest.InRequest(t, ten, a, func(ctx context.Context) (err error) {
			r, err = configuration.Settings{}.Rules(ctx, "purchase_order")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := activeRules(f.Tenant, f.Tenant.Human); r.ConfigVersion != 1 || r.Schema == nil || r.Schema.Version != 1 || len(r.Rules) != 0 {
		t.Errorf("purchase order rules: got %+v, want version 1 with the schema and no rules", r)
	}
	if r := activeRules(bare.ten, bare.ten.Human); r.ConfigVersion != 0 || r.Schema != nil {
		t.Errorf("tenant without configuration: got %+v, want none", r)
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "rules", func(ctx context.Context) error {
		_, err := configuration.Settings{}.Rules(ctx, "purchase_order")
		return err
	})
	ranges, _ := f.Must(200, "GET", "/v1/number-ranges", "", "")
	items, _ := ranges["items"].([]any)
	if len(items) == 0 {
		t.Fatal("the fixture has no number ranges")
	}
	r := items[0].(map[string]any)
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "next number", func(ctx context.Context) error {
		_, err := configuration.Numbers{}.Next(ctx, uuid.MustParse(r["legal_entity_id"].(string)), r["document_type"].(string), int16(r["fiscal_year"].(float64)))
		return err
	})
}

func TestApprovalFailures(t *testing.T) {
	f := fixture.New(t, perms...)
	doc := fixture.Document(t)
	doc.PaymentTerms[0].NetDays += 1
	b, _ := json.Marshal(doc)
	propose := func() map[string]any {
		cr, _ := f.Must(201, "POST", "/v1/change-requests", `{"title": "Terms", "base_version": 1, "proposed": `+string(b)+`}`, "")
		return cr
	}
	pending := propose()
	f.Must(200, "POST", "/v1/change-requests/"+pending["id"].(string)+":submit", "", `"1"`)
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "withdraw pending", func(ctx context.Context) error {
		_, err := configuration.WithdrawChangeRequest(ctx, fixture.Deps(), uuid.MustParse(pending["id"].(string)), func(int32) error { return nil }, "r")
		return err
	})
	if _, err := (configuration.Numbers{}).Next(context.Background(), uuid.New(), "journal_sa", 2026); err == nil {
		t.Error("number outside a unit of work")
	}
	// Without the approver role of the policy, submission fails.
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND code = 'process_owner'`, f.Tenant.ID)
	draft := propose()
	f.Problem(422, "workflow.unknown_role", "POST", "/v1/change-requests/"+draft["id"].(string)+":submit", "", `"1"`)
}
