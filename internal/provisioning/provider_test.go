package provisioning_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// claimed records the claims provisioning makes, and answers with err.
type claimed struct {
	tenants, providers []uuid.UUID
	err                error
}

func (c *claimed) claim(_ context.Context, tenant, provider uuid.UUID, _, _ string) error {
	c.tenants, c.providers = append(c.tenants, tenant), append(c.providers, provider)
	return c.err
}

// TestProvisioningWithAProvider: a tenant provisioned with its identity
// provider has it registered, published and audited in the same
// transaction, its issuer claimed before the commit, and its administrator
// bound to it (ADR-0031).
func TestProvisioningWithAProvider(t *testing.T) {
	ctx := context.Background()
	p := provisioner(t)
	c := &claimed{}
	p.Claim, p.Production = c.claim, true
	var probed []string
	p.Discover = func(_ context.Context, issuer string) error { probed = append(probed, issuer); return nil }
	r := request(t)
	issuer := "https://acme-" + uuid.NewString() + ".example.com"
	r.IdentityProvider = &identity.NewIdentityProvider{Issuer: issuer, Audience: "erp-api", AMRValues: []string{"mfa"}}
	res, err := p.Provision(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.IdentityProviderID == nil {
		t.Fatal("result: got no identity provider, want the registered one")
	}
	var adminIssuer string
	pgtest.Scan(t, `SELECT external_issuer FROM identity.principal WHERE tenant_id = $1 AND id = $2`, []any{res.TenantID, res.AdministratorID}, &adminIssuer)
	if adminIssuer != issuer {
		t.Errorf("administrator's issuer: got %s, want %s", adminIssuer, issuer)
	}
	if len(probed) != 1 || probed[0] != issuer {
		t.Errorf("probed: got %v, want %s", probed, issuer)
	}
	if len(c.providers) != 1 || c.providers[0] != *res.IdentityProviderID || c.tenants[0] != res.TenantID {
		t.Errorf("claims: got %v of %v, want the provider of the tenant", c.providers, c.tenants)
	}
	events := apitest.Published(t, pgtest.Tenant{ID: res.TenantID}, outbox.Event{Context: "identity", Name: "identity_provider_registered"})
	if len(events) != 1 || events[0]["issuer"] != issuer {
		t.Errorf("events: got %v, want one for %s", events, issuer)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'identity.identity_provider.register'`, []any{res.TenantID}, &audited)
	if audited != 1 {
		t.Errorf("registrations audited: got %d, want 1", audited)
	}
}

func TestProvisioningProviderRefusals(t *testing.T) {
	ctx := context.Background()
	with := func(issuer string) provisioning.Request {
		r := request(t)
		r.IdentityProvider = &identity.NewIdentityProvider{Issuer: issuer, Audience: "erp-api"}
		return r
	}
	t.Run("production needs a provider", func(t *testing.T) {
		p := provisioner(t)
		p.Production = true
		_, err := p.Provision(ctx, request(t))
		if got := problem.From(err).Code; got != "provisioning.identity_provider_required" {
			t.Errorf("got %q (%v), want provisioning.identity_provider_required", got, err)
		}
	})
	t.Run("production needs https", func(t *testing.T) {
		p := provisioner(t)
		p.Production = true
		_, err := p.Provision(ctx, with("http://localhost:8080/"+uuid.NewString()))
		if got := problem.From(err).Code; got != "identity.invalid_provider" {
			t.Errorf("got %q (%v), want identity.invalid_provider", got, err)
		}
	})
	t.Run("an unreachable provider", func(t *testing.T) {
		p := provisioner(t)
		p.Discover = func(context.Context, string) error { return errors.New("connection refused") }
		_, err := p.Provision(ctx, with("https://down-"+uuid.NewString()+".example.com"))
		if got := problem.From(err).Code; got != "identity.provider_unreachable" {
			t.Errorf("got %q (%v), want identity.provider_unreachable", got, err)
		}
	})
	t.Run("an invalid provider", func(t *testing.T) {
		_, err := provisioner(t).Provision(ctx, with("ftp://idp.example.com"))
		if got := problem.From(err).Code; got != "identity.invalid_provider" {
			t.Errorf("got %q (%v), want identity.invalid_provider", got, err)
		}
	})
	t.Run("a claim refused leaves no tenant", func(t *testing.T) {
		p := provisioner(t)
		c := &claimed{err: problem.Conflict("identity.provider_taken", "taken")}
		p.Claim = c.claim
		_, err := p.Provision(ctx, with("https://taken-"+uuid.NewString()+".example.com"))
		if got := problem.From(err).Code; got != "identity.provider_taken" {
			t.Fatalf("got %q (%v), want identity.provider_taken", got, err)
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1`, []any{c.tenants[0]}, &n)
		if n != 0 {
			t.Errorf("tenants left: got %d, want 0", n)
		}
	})
}
