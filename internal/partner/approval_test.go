package partner_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
)

// TestChangesThroughApproval decides partner changes on their approval
// requests: approved changes apply, rejected and withdrawn ones do not.
func TestChangesThroughApproval(t *testing.T) {
	f := fixture.New(t, perms...)
	create := func(name string) (string, string) {
		p, _ := f.Must(201, "POST", "/v1/business-partners", `{"kind": "organization", "name": "`+name+`", "country_code": "DE", "roles": ["customer"], "reason": "New"}`, "")
		changes, _ := f.Must(200, "GET", "/v1/partner-changes?partner_id="+p["id"].(string), "", "")
		return p["id"].(string), changes["items"].([]any)[0].(map[string]any)["id"].(string)
	}
	approved, change := create("Approved GmbH")
	f.Approve(t, change)
	if p, _ := f.Must(200, "GET", "/v1/business-partners/"+approved, "", ""); p["status"] != "active" {
		t.Fatalf("approved: %v", p)
	}
	_, rejected := create("Rejected GmbH")
	r := f.Pending(t, rejected)
	f.As(f.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Duplicate"}`, `"1"`)
	if c, _ := f.Must(200, "GET", "/v1/partner-changes/"+rejected, "", ""); c["status"] != "rejected" {
		t.Fatalf("rejected: %v", c)
	}
	_, withdrawn := create("Withdrawn GmbH")
	w := f.Pending(t, withdrawn)
	f.Must(200, "POST", "/v1/approval-requests/"+w["id"].(string)+":cancel", `{"comment": "Not needed"}`, `"1"`)
	if c, _ := f.Must(200, "GET", "/v1/partner-changes/"+withdrawn, "", ""); c["status"] != "cancelled" {
		t.Fatalf("withdrawn: %v", c)
	}
	// Unblocking goes through approval too.
	_, tag := f.Must(200, "GET", "/v1/business-partners/"+approved, "", "")
	f.Must(200, "POST", "/v1/business-partners/"+approved+":block", `{"reason": "Check"}`, tag)
	_, tag = f.Must(200, "GET", "/v1/business-partners/"+approved, "", "")
	unblock, _ := f.Must(200, "POST", "/v1/business-partners/"+approved+":unblock", `{"reason": "Checked"}`, tag)
	f.Approve(t, unblock["id"].(string))
	if p, _ := f.Must(200, "GET", "/v1/business-partners/"+approved, "", ""); p["status"] != "active" {
		t.Fatalf("unblocked: %v", p)
	}
	for name, call := range map[string]func(context.Context) error{
		"approved": func(ctx context.Context) error { return partner.Approvals{}.Approved(ctx, uuid.New(), uuid.New()) },
		"rejected": func(ctx context.Context) error { return partner.Approvals{}.Rejected(ctx, uuid.New(), uuid.New(), "c") },
		"cancelled": func(ctx context.Context) error {
			return partner.Approvals{}.Cancelled(ctx, uuid.New(), uuid.New(), "c")
		},
	} {
		if call(context.Background()) == nil {
			t.Errorf("%s outside a unit of work", name)
		}
	}
}

// noApproval is a workflow that needs no approval for anything.
type noApproval struct{}

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func TestRequestApproval(t *testing.T) {
	f := fixture.New(t, perms...)
	var c partner.Change
	err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
		_, c, err = partner.CreatePartner(ctx, partner.Ports{LegalEntities: nil, Keys: apitest.FieldKeys()},
			partner.NewPartner{Kind: "person", Name: "Instant", CountryCode: "DE", Roles: []string{"customer"}, Reason: "r"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		applied, err := partner.RequestApproval(ctx, noApproval{}, c)
		if err == nil && applied.Status != partner.ChangeApplied {
			t.Errorf("without approval steps: %+v", applied)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partner.RequestApproval(context.Background(), fixture.Approvals(), c); err == nil {
		t.Error("outside a unit of work")
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "request approval", func(ctx context.Context) error {
		_, err := partner.RequestApproval(ctx, fixture.Approvals(), c)
		return err
	})
}

func TestSuppliersPort(t *testing.T) {
	f := fixture.New(t, perms...)
	s := partner.Suppliers{}
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		sup, ok, err := s.Supplier(ctx, f.Supplier, f.DE)
		if err != nil || !ok || !sup.SetUp || sup.PaymentTermCode != "NT30" || sup.ReconciliationID != f.Account(t, "2000", "211000") || sup.Usable() != "" {
			t.Errorf("supplier: %+v %v %v", sup, ok, err)
		}
		if sup, _, _ := s.Supplier(ctx, f.Supplier, uuid.New()); sup.Usable() != "the supplier is not set up in this legal entity" {
			t.Errorf("other legal entity: %q", sup.Usable())
		}
		if _, ok, err := s.Supplier(ctx, uuid.New(), f.DE); ok || err != nil {
			t.Errorf("unknown: %v %v", ok, err)
		}
		for want, sup := range map[string]partner.Supplier{
			"the business partner is pending":                          {Status: "pending"},
			"the business partner is not a supplier":                   {Status: "active"},
			"the supplier is blocked for posting in this legal entity": {Status: "active", IsSupplier: true, SetUp: true, IsPostingBlocked: true},
		} {
			if got := sup.Usable(); got != want {
				t.Errorf("usable: %q, want %q", got, want)
			}
		}
		if ok, err := s.PaymentTermExists(ctx, "NT30"); !ok || err != nil {
			t.Errorf("NT30: %v %v", ok, err)
		}
		if ok, _ := s.PaymentTermExists(ctx, "NT99"); ok {
			t.Error("NT99 exists")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"supplier": func(ctx context.Context) error { _, _, err := s.Supplier(ctx, f.Supplier, f.DE); return err },
		"terms":    func(ctx context.Context) error { _, err := s.PaymentTermExists(ctx, "NT30"); return err },
	} {
		if call(context.Background()) == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, name, call)
	}
}
