package partner_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
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
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
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
		if err != nil || !ok {
			t.Errorf("supplier: got %v %v, want found", ok, err)
		}
		if !sup.SetUp {
			t.Error("supplier set up: got false, want true")
		}
		if sup.PaymentTermCode != "NT30" {
			t.Errorf("supplier payment term: got %s, want NT30", sup.PaymentTermCode)
		}
		if want := f.Account(t, "2000", "211000"); sup.ReconciliationID != want {
			t.Errorf("supplier reconciliation account: got %v, want %v", sup.ReconciliationID, want)
		}
		if got := sup.Usable(); got != "" {
			t.Errorf("supplier usable: got %q, want \"\"", got)
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
			t.Run(want, func(t *testing.T) {
				if got := sup.Usable(); got != want {
					t.Errorf("got %q, want %q", got, want)
				}
			})
		}
		if ok, err := s.PaymentTermExists(ctx, "NT30"); !ok || err != nil {
			t.Errorf("NT30: %v %v", ok, err)
		}
		if ok, _ := s.PaymentTermExists(ctx, "NT99"); ok {
			t.Error("NT99 exists")
		}
		if term, ok, err := s.PaymentTerm(ctx, "NT30"); !ok || err != nil || term.NetDays != 30 || len(term.Discounts) != 0 {
			t.Errorf("payment term NT30: got %+v %v %v, want net 30 days without discounts", term, ok, err)
		}
		if _, ok, err := s.PaymentTerm(ctx, "NT99"); ok || err != nil {
			t.Errorf("payment term NT99: got %v %v, want not found", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"supplier": func(ctx context.Context) error { _, _, err := s.Supplier(ctx, f.Supplier, f.DE); return err },
		"terms":    func(ctx context.Context) error { _, err := s.PaymentTermExists(ctx, "NT30"); return err },
		"term":     func(ctx context.Context) error { _, _, err := s.PaymentTerm(ctx, "NT30"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, name, call)
		})
	}
}
