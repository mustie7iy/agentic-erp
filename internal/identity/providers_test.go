package identity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// registry runs the identity provider operations of one tenant, each in a
// transaction of its own, as the control plane does (ADR-0031).
type registry struct {
	t      *testing.T
	pool   *database.Pool
	tenant uuid.UUID
	actor  uuid.UUID
}

func newRegistry(t *testing.T) registry {
	t.Helper()
	return registry{t: t, pool: pgtest.Pool(t), tenant: pgtest.NewTenant(t).ID, actor: uuid.Must(uuid.NewV7())}
}

// in runs fn in the tenant's scope as the application role and commits
// when fn succeeds.
func (r registry) in(fn func(ctx context.Context, tx pgx.Tx) error) error {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: r.tenant})
	if err != nil {
		r.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r registry) register(in identity.NewIdentityProvider) (identity.IdentityProvider, error) {
	var p identity.IdentityProvider
	err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = identity.RegisterIdentityProvider(ctx, tx, r.actor, in)
		return err
	})
	return p, err
}

func (r registry) mustRegister(issuer, audience string, amr ...string) identity.IdentityProvider {
	r.t.Helper()
	p, err := r.register(identity.NewIdentityProvider{Issuer: issuer, Audience: audience, AMRValues: amr})
	if err != nil {
		r.t.Fatalf("register %s %s: %v", issuer, audience, err)
	}
	return p
}

func (r registry) update(id uuid.UUID, ifMatch string, ch identity.IdentityProviderChange) (identity.IdentityProvider, error) {
	var p identity.IdentityProvider
	err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = identity.UpdateIdentityProvider(ctx, tx, r.actor, id, etag.IfMatch(ifMatch), ch)
		return err
	})
	return p, err
}

func (r registry) retire(id uuid.UUID, ifMatch string) (identity.IdentityProvider, error) {
	var p identity.IdentityProvider
	err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = identity.RetireIdentityProvider(ctx, tx, r.actor, id, etag.IfMatch(ifMatch))
		return err
	})
	return p, err
}

func (r registry) policy() identity.AuthPolicy {
	r.t.Helper()
	var p identity.AuthPolicy
	if err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = identity.ReadAuthPolicy(ctx, tx)
		return err
	}); err != nil {
		r.t.Fatalf("read policy: %v", err)
	}
	return p
}

func (r registry) updatePolicy(ifMatch string, ch identity.AuthPolicyChange) (identity.AuthPolicy, error) {
	var p identity.AuthPolicy
	err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
		p, err = identity.UpdateAuthPolicy(ctx, tx, r.actor, etag.IfMatch(ifMatch), ch)
		return err
	})
	return p, err
}

// issuer is an issuer URL no other test registers.
func issuer(name string) string {
	return "https://idp.example.com/" + name + "/" + uuid.NewString()
}

func wantProblem(t *testing.T, err error, code string) {
	t.Helper()
	if got := problem.From(err).Code; got != code {
		t.Errorf("problem: got %q (%v), want %q", got, err, code)
	}
}

func TestRegisterIdentityProvider(t *testing.T) {
	r := newRegistry(t)
	claim := "erp_tenant"
	iss := issuer("register")
	p, err := r.register(identity.NewIdentityProvider{Issuer: iss, Audience: "erp", TenantClaim: &claim, ACRValues: []string{"urn:mfa"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Run("as given", func(t *testing.T) {
		if p.Issuer != iss || p.Audience != "erp" || p.TenantClaim == nil || *p.TenantClaim != claim {
			t.Errorf("provider: got %+v, want issuer %s, audience erp, tenant claim %s", p, iss, claim)
		}
		if got := strings.Join(p.ACRValues, ","); got != "urn:mfa" {
			t.Errorf("acr_values: got %q, want urn:mfa", got)
		}
		if p.AMRValues == nil || len(p.AMRValues) != 0 {
			t.Errorf("amr_values: got %#v, want empty", p.AMRValues)
		}
	})
	t.Run("active, version 1, by the actor", func(t *testing.T) {
		if p.Status != identity.ProviderActive || p.Version != 1 || p.RetiredAt != nil {
			t.Errorf("status, version, retired_at: got %s, %d, %v, want active, 1, nil", p.Status, p.Version, p.RetiredAt)
		}
		if p.CreatedBy != r.actor || p.UpdatedBy != r.actor || p.TenantID != r.tenant {
			t.Errorf("created_by, updated_by, tenant: got %s, %s, %s, want %s, %s, %s", p.CreatedBy, p.UpdatedBy, p.TenantID, r.actor, r.actor, r.tenant)
		}
	})
	t.Run("read back", func(t *testing.T) {
		var got identity.IdentityProvider
		if err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
			got, err = identity.GetIdentityProvider(ctx, tx, p.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.ID != p.ID || got.Issuer != iss || got.Version != 1 {
			t.Errorf("read back: got %+v, want %+v", got, p)
		}
	})
	t.Run("an issuer on loopback may be http", func(t *testing.T) {
		for _, u := range []string{"http://localhost:8080/realms/" + uuid.NewString(), "http://127.0.0.1:9000/" + uuid.NewString(), "http://[::1]/" + uuid.NewString()} {
			if _, err := r.register(identity.NewIdentityProvider{Issuer: u, Audience: "erp"}); err != nil {
				t.Errorf("%s: got %v, want registered", u, err)
			}
		}
	})
	t.Run("taken by another tenant", func(t *testing.T) {
		other := newRegistry(t)
		_, err := other.register(identity.NewIdentityProvider{Issuer: iss, Audience: "erp"})
		wantProblem(t, err, "identity.provider_taken")
	})
}

func TestInvalidIdentityProviders(t *testing.T) {
	r := newRegistry(t)
	long := func(n int) string { return strings.Repeat("a", n) }
	bad := "1claim"
	for _, c := range []struct {
		name  string
		in    identity.NewIdentityProvider
		field string
	}{
		{"http off loopback", identity.NewIdentityProvider{Issuer: "http://idp.example.com", Audience: "erp"}, "issuer"},
		{"another scheme", identity.NewIdentityProvider{Issuer: "ftp://idp.example.com", Audience: "erp"}, "issuer"},
		{"no host", identity.NewIdentityProvider{Issuer: "https:///path", Audience: "erp"}, "issuer"},
		{"a query", identity.NewIdentityProvider{Issuer: "https://idp.example.com/?tenant=a", Audience: "erp"}, "issuer"},
		{"a fragment", identity.NewIdentityProvider{Issuer: "https://idp.example.com/#a", Audience: "erp"}, "issuer"},
		{"user info", identity.NewIdentityProvider{Issuer: "https://u:p@idp.example.com", Audience: "erp"}, "issuer"},
		{"spaces", identity.NewIdentityProvider{Issuer: "https://idp.example.com/a b", Audience: "erp"}, "issuer"},
		{"not a URL", identity.NewIdentityProvider{Issuer: "https://idp.example.com/%zz", Audience: "erp"}, "issuer"},
		{"issuer too long", identity.NewIdentityProvider{Issuer: "https://idp.example.com/" + long(500), Audience: "erp"}, "issuer"},
		{"no audience", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: ""}, "audience"},
		{"audience too long", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: long(256)}, "audience"},
		{"tenant claim", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: "erp", TenantClaim: &bad}, "tenant_claim"},
		{"too many acr values", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: "erp", ACRValues: strings.Split(long(17), "")}, "acr_values"},
		{"empty acr value", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: "erp", ACRValues: []string{""}}, "acr_values"},
		{"acr value too long", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: "erp", ACRValues: []string{long(256)}}, "acr_values"},
		{"amr value too long", identity.NewIdentityProvider{Issuer: issuer("x"), Audience: "erp", AMRValues: []string{long(65)}}, "amr_values"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.register(c.in)
			p := problem.From(err)
			if p.Code != "identity.invalid_provider" {
				t.Fatalf("problem: got %q (%v), want identity.invalid_provider", p.Code, err)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field errors: got %+v, want one on %s", p.Errors, c.field)
			}
		})
	}
}

func TestUpdateIdentityProvider(t *testing.T) {
	r := newRegistry(t)
	p := r.mustRegister(issuer("update"), "erp")
	claim := "tid"
	set := &claim
	p, err := r.update(p.ID, etag.Format(p.Version), identity.IdentityProviderChange{TenantClaim: &set, AMRValues: &[]string{"mfa", "hwk"}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	t.Run("changed", func(t *testing.T) {
		if p.TenantClaim == nil || *p.TenantClaim != claim || strings.Join(p.AMRValues, ",") != "mfa,hwk" || p.Version != 2 {
			t.Errorf("provider: got claim %v, amr %v, version %d, want tid, mfa,hwk, 2", p.TenantClaim, p.AMRValues, p.Version)
		}
	})
	t.Run("absent fields unchanged, null clears", func(t *testing.T) {
		var clear *string
		p, err = r.update(p.ID, etag.Format(p.Version), identity.IdentityProviderChange{TenantClaim: &clear, ACRValues: &[]string{"urn:loa:2"}})
		if err != nil {
			t.Fatal(err)
		}
		if p.TenantClaim != nil || strings.Join(p.AMRValues, ",") != "mfa,hwk" || strings.Join(p.ACRValues, ",") != "urn:loa:2" {
			t.Errorf("provider: got claim %v, amr %v, acr %v, want nil, mfa,hwk, urn:loa:2", p.TenantClaim, p.AMRValues, p.ACRValues)
		}
	})
	t.Run("stale version", func(t *testing.T) {
		_, err := r.update(p.ID, `"1"`, identity.IdentityProviderChange{})
		if got := problem.From(err).Status; got != 412 {
			t.Errorf("status: got %d (%v), want 412", got, err)
		}
	})
	t.Run("invalid values", func(t *testing.T) {
		_, err := r.update(p.ID, "*", identity.IdentityProviderChange{AMRValues: &[]string{""}})
		wantProblem(t, err, "identity.invalid_provider")
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := r.update(uuid.Must(uuid.NewV7()), "*", identity.IdentityProviderChange{})
		if got := problem.From(err).Status; got != 404 {
			t.Errorf("status: got %d (%v), want 404", got, err)
		}
	})
}

func TestRetireIdentityProvider(t *testing.T) {
	r := newRegistry(t)
	first := r.mustRegister(issuer("first"), "erp")
	t.Run("the last active provider stays", func(t *testing.T) {
		_, err := r.retire(first.ID, "*")
		wantProblem(t, err, "identity.last_provider")
	})
	second := r.mustRegister(issuer("second"), "erp")
	retired, err := r.retire(first.ID, etag.Format(first.Version))
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	t.Run("retired", func(t *testing.T) {
		if retired.Status != identity.ProviderRetired || retired.RetiredAt == nil || retired.Version != 2 {
			t.Errorf("provider: got %s, %v, version %d, want retired, a time, 2", retired.Status, retired.RetiredAt, retired.Version)
		}
	})
	t.Run("a retired provider does not change", func(t *testing.T) {
		_, err := r.retire(first.ID, "*")
		wantProblem(t, err, "identity.provider_retired")
		_, err = r.update(first.ID, "*", identity.IdentityProviderChange{AMRValues: &[]string{"mfa"}})
		wantProblem(t, err, "identity.provider_retired")
	})
	t.Run("stale version", func(t *testing.T) {
		_, err := r.retire(second.ID, `"7"`)
		if got := problem.From(err).Status; got != 412 {
			t.Errorf("status: got %d (%v), want 412", got, err)
		}
	})
	t.Run("listed, retired too", func(t *testing.T) {
		var ps []identity.IdentityProvider
		if err := r.in(func(ctx context.Context, tx pgx.Tx) (err error) {
			ps, err = identity.ListIdentityProviders(ctx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(ps) != 2 || ps[0].ID != first.ID || ps[1].ID != second.ID {
			t.Errorf("providers: got %+v, want first then second", ps)
		}
	})
}

func TestAuthPolicyDefaults(t *testing.T) {
	r := newRegistry(t)
	p := r.policy()
	if p.MFARequired || p.SessionMaxAge != 12*time.Hour || p.IdleTimeout != 30*time.Minute {
		t.Errorf("defaults: got mfa %v, max age %v, idle %v, want false, 12h, 30m", p.MFARequired, p.SessionMaxAge, p.IdleTimeout)
	}
	if p.Version != 1 || p.UpdatedBy != nil {
		t.Errorf("version, updated_by: got %d, %v, want 1, nil", p.Version, p.UpdatedBy)
	}
}

func TestUpdateAuthPolicy(t *testing.T) {
	r := newRegistry(t)
	plain := r.mustRegister(issuer("plain"), "erp")
	yes := true
	t.Run("multi-factor no provider can prove", func(t *testing.T) {
		_, err := r.updatePolicy("*", identity.AuthPolicyChange{MFARequired: &yes})
		wantProblem(t, err, "identity.mfa_unprovable")
	})
	if _, err := r.update(plain.ID, "*", identity.IdentityProviderChange{AMRValues: &[]string{"mfa"}}); err != nil {
		t.Fatal(err)
	}
	age, idle := 8*time.Hour, 15*time.Minute
	p, err := r.updatePolicy(`"1"`, identity.AuthPolicyChange{MFARequired: &yes, SessionMaxAge: &age, IdleTimeout: &idle})
	if err != nil {
		t.Fatalf("update policy: %v", err)
	}
	t.Run("changed", func(t *testing.T) {
		if !p.MFARequired || p.SessionMaxAge != age || p.IdleTimeout != idle || p.Version != 2 || p.UpdatedBy == nil || *p.UpdatedBy != r.actor {
			t.Errorf("policy: got %+v, want mfa, 8h, 15m, version 2, by %s", p, r.actor)
		}
		if got := r.policy(); got.Version != 2 || got.IdleTimeout != idle {
			t.Errorf("read back: got %+v, want version 2, idle 15m", got)
		}
	})
	t.Run("absent fields unchanged", func(t *testing.T) {
		shorter := 10 * time.Minute
		p, err := r.updatePolicy("*", identity.AuthPolicyChange{IdleTimeout: &shorter})
		if err != nil {
			t.Fatal(err)
		}
		if !p.MFARequired || p.SessionMaxAge != age || p.IdleTimeout != shorter {
			t.Errorf("policy: got %+v, want mfa, 8h, 10m", p)
		}
	})
	t.Run("a provider that cannot prove it", func(t *testing.T) {
		_, err := r.register(identity.NewIdentityProvider{Issuer: issuer("plain"), Audience: "erp"})
		wantProblem(t, err, "identity.mfa_unprovable")
		_, err = r.update(plain.ID, "*", identity.IdentityProviderChange{AMRValues: &[]string{}})
		wantProblem(t, err, "identity.mfa_unprovable")
	})
	t.Run("stale version", func(t *testing.T) {
		_, err := r.updatePolicy(`"1"`, identity.AuthPolicyChange{})
		if got := problem.From(err).Status; got != 412 {
			t.Errorf("status: got %d (%v), want 412", got, err)
		}
	})
}

func TestInvalidAuthPolicies(t *testing.T) {
	r := newRegistry(t)
	d := func(v time.Duration) *time.Duration { return &v }
	for _, c := range []struct {
		name  string
		ch    identity.AuthPolicyChange
		field string
	}{
		{"lifetime too short", identity.AuthPolicyChange{SessionMaxAge: d(14 * time.Minute), IdleTimeout: d(5 * time.Minute)}, "session_max_age"},
		{"lifetime too long", identity.AuthPolicyChange{SessionMaxAge: d(25 * time.Hour)}, "session_max_age"},
		{"lifetime in parts of a second", identity.AuthPolicyChange{SessionMaxAge: d(time.Hour + time.Millisecond)}, "session_max_age"},
		{"idle too short", identity.AuthPolicyChange{IdleTimeout: d(4 * time.Minute)}, "idle_timeout"},
		{"idle too long", identity.AuthPolicyChange{SessionMaxAge: d(24 * time.Hour), IdleTimeout: d(9 * time.Hour)}, "idle_timeout"},
		{"idle beyond the lifetime", identity.AuthPolicyChange{SessionMaxAge: d(time.Hour), IdleTimeout: d(2 * time.Hour)}, "idle_timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.updatePolicy("*", c.ch)
			p := problem.From(err)
			if p.Code != "identity.invalid_policy" {
				t.Fatalf("problem: got %q (%v), want identity.invalid_policy", p.Code, err)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field errors: got %+v, want one on %s", p.Errors, c.field)
			}
		})
	}
}

// resolve runs ResolveIdentityProvider as the cell does before it knows the
// tenant: as the control role, across tenants, read-only.
func resolve(t *testing.T, pool *database.Pool, iss string, audiences ...string) (identity.Registration, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{Role: "erp_control", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	return identity.ResolveIdentityProvider(ctx, tx, iss, audiences)
}

func TestResolveIdentityProvider(t *testing.T) {
	r := newRegistry(t)
	iss := issuer("resolve")
	p := r.mustRegister(iss, "erp", "mfa")
	t.Run("the registration decides the tenant, with the default policy", func(t *testing.T) {
		reg, err := resolve(t, r.pool, iss, "other", "erp")
		if err != nil {
			t.Fatal(err)
		}
		if reg.TenantID != r.tenant || reg.ProviderID != p.ID || reg.Audience != "erp" || strings.Join(reg.AMRValues, ",") != "mfa" {
			t.Errorf("registration: got %+v, want tenant %s, provider %s, audience erp, amr mfa", reg, r.tenant, p.ID)
		}
		if reg.Policy.MFARequired || reg.Policy.SessionMaxAge != 12*time.Hour || reg.Policy.IdleTimeout != 30*time.Minute {
			t.Errorf("policy: got %+v, want the defaults", reg.Policy)
		}
	})
	t.Run("with the tenant's policy", func(t *testing.T) {
		yes, idle := true, 10*time.Minute
		if _, err := r.updatePolicy("*", identity.AuthPolicyChange{MFARequired: &yes, IdleTimeout: &idle}); err != nil {
			t.Fatal(err)
		}
		reg, err := resolve(t, r.pool, iss, "erp")
		if err != nil {
			t.Fatal(err)
		}
		if !reg.Policy.MFARequired || reg.Policy.IdleTimeout != idle {
			t.Errorf("policy: got %+v, want mfa, idle 10m", reg.Policy)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		for name, auds := range map[string][]string{"issuer": nil, "audience": {"nobody"}} {
			i := iss
			if name == "issuer" {
				i, auds = issuer("nobody"), []string{"erp"}
			}
			if _, err := resolve(t, r.pool, i, auds...); !errors.Is(err, identity.ErrUnknownProvider) {
				t.Errorf("unknown %s: got %v, want ErrUnknownProvider", name, err)
			}
		}
	})
	t.Run("two tenants' audiences in one token", func(t *testing.T) {
		other := newRegistry(t)
		other.mustRegister(iss, "erp-other")
		if _, err := resolve(t, r.pool, iss, "erp", "erp-other"); !errors.Is(err, identity.ErrAmbiguousProvider) {
			t.Errorf("got %v, want ErrAmbiguousProvider", err)
		}
	})
	t.Run("a retired provider resolves no more", func(t *testing.T) {
		r.mustRegister(issuer("spare"), "erp", "mfa")
		if _, err := r.retire(p.ID, "*"); err != nil {
			t.Fatal(err)
		}
		if _, err := resolve(t, r.pool, iss, "erp"); !errors.Is(err, identity.ErrUnknownProvider) {
			t.Errorf("got %v, want ErrUnknownProvider", err)
		}
	})
}

// TestIdentityProviderDatabaseFailures fails each statement of every
// operation in turn: the error reaches the caller, wrapped, and nothing is
// committed.
func TestIdentityProviderDatabaseFailures(t *testing.T) {
	r := newRegistry(t)
	p := r.mustRegister(issuer("failures"), "erp", "mfa")
	r.mustRegister(issuer("failures-spare"), "erp", "mfa")
	yes := true
	ops := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"register": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RegisterIdentityProvider(ctx, tx, r.actor, identity.NewIdentityProvider{Issuer: issuer("f"), Audience: "erp", AMRValues: []string{"mfa"}})
			return err
		},
		"register without values": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RegisterIdentityProvider(ctx, tx, r.actor, identity.NewIdentityProvider{Issuer: issuer("f"), Audience: "erp"})
			return err
		},
		"get": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.GetIdentityProvider(ctx, tx, p.ID)
			return err
		},
		"list": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.ListIdentityProviders(ctx, tx)
			return err
		},
		"update": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.UpdateIdentityProvider(ctx, tx, r.actor, p.ID, etag.IfMatch("*"), identity.IdentityProviderChange{AMRValues: &[]string{"otp"}})
			return err
		},
		"retire": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RetireIdentityProvider(ctx, tx, r.actor, p.ID, etag.IfMatch("*"))
			return err
		},
		"read policy": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.ReadAuthPolicy(ctx, tx)
			return err
		},
		"update policy": func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.UpdateAuthPolicy(ctx, tx, r.actor, etag.IfMatch("*"), identity.AuthPolicyChange{MFARequired: &yes})
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			for n := 1; ; n++ {
				ctx := context.Background()
				tx, err := r.pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: r.tenant})
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
	t.Run("resolve", func(t *testing.T) {
		ctx := context.Background()
		tx, err := r.pool.Begin(ctx, database.Scope{Role: "erp_control", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := identity.ResolveIdentityProvider(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, p.Issuer, []string{"erp"}); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected error", err)
		}
	})
}

// begin opens a transaction of the registry's tenant that the test commits
// or rolls back itself, to interleave two of them.
func (r registry) begin() pgx.Tx {
	r.t.Helper()
	tx, err := r.pool.Begin(context.Background(), database.Scope{Role: "erp_app", TenantID: r.tenant})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = tx.Rollback(context.Background()) }) // a no-op once committed
	return tx
}

// interleave runs first in a transaction left open, then second in another,
// which waits on a lock first holds (or, if it does not, finishes first),
// commits the first and returns the second's result. The wait is read off
// the first's backend, so another package's test waiting on a lock in the
// same server cannot pass for it.
func interleave(t *testing.T, r registry, first, second func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	// Cleanups run last first: tx1, begun second, rolls back first when the
	// test fails, releasing the locks tx2 may be waiting on.
	tx2 := r.begin()
	tx1 := r.begin()
	var pid int32
	if err := tx1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := first(ctx, tx1); err != nil {
		t.Fatalf("first: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		err := second(ctx, tx2)
		if err == nil {
			err = tx2.Commit(ctx)
		}
		done <- err
	}()
	var early error
	finished := false
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case early = <-done:
			finished = true
			return true
		default:
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}, "the second transaction neither waits on the first nor finishes")
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit first: %v", err)
	}
	if finished {
		return early
	}
	return <-done
}

// TestMultiFactorRuleUnderConcurrency: requiring multi-factor and adding a
// provider that cannot prove it, in two transactions at once, never both
// succeed, whichever comes first (ADR-0031): with the tenant's policy row
// still to be created, and with it there, when only its row locks order
// the two.
func TestMultiFactorRuleUnderConcurrency(t *testing.T) {
	yes := true
	requireMFA := func(r registry) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.UpdateAuthPolicy(ctx, tx, r.actor, etag.IfMatch("*"), identity.AuthPolicyChange{MFARequired: &yes})
			return err
		}
	}
	addPlain := func(r registry) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RegisterIdentityProvider(ctx, tx, r.actor, identity.NewIdentityProvider{Issuer: issuer("plain"), Audience: "erp"})
			return err
		}
	}
	clearValues := func(r registry, p identity.IdentityProvider) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.UpdateIdentityProvider(ctx, tx, r.actor, p.ID, etag.IfMatch("*"), identity.IdentityProviderChange{AMRValues: &[]string{}})
			return err
		}
	}
	for _, policyRow := range []bool{false, true} {
		setup := func(t *testing.T) (registry, identity.IdentityProvider) {
			t.Helper()
			r := newRegistry(t)
			p := r.mustRegister(issuer("mfa"), "erp", "mfa")
			if policyRow {
				age := 10 * time.Hour
				if _, err := r.updatePolicy("*", identity.AuthPolicyChange{SessionMaxAge: &age}); err != nil {
					t.Fatal(err)
				}
			}
			return r, p
		}
		name := map[bool]string{false: "no policy row", true: "policy row"}[policyRow]
		// With the policy row there, "policy first" and "policy before
		// values cleared" are ordered by the second's insert of the row
		// waiting on the first's update of it; the other two only by the
		// row locks.
		t.Run(name+", policy first", func(t *testing.T) {
			r, _ := setup(t)
			wantProblem(t, interleave(t, r, requireMFA(r), addPlain(r)), "identity.mfa_unprovable")
		})
		t.Run(name+", provider first", func(t *testing.T) {
			r, _ := setup(t)
			wantProblem(t, interleave(t, r, addPlain(r), requireMFA(r)), "identity.mfa_unprovable")
		})
		t.Run(name+", values cleared first", func(t *testing.T) {
			r, p := setup(t)
			wantProblem(t, interleave(t, r, clearValues(r, p), requireMFA(r)), "identity.mfa_unprovable")
		})
		t.Run(name+", policy before values cleared", func(t *testing.T) {
			r, p := setup(t)
			wantProblem(t, interleave(t, r, requireMFA(r), clearValues(r, p)), "identity.mfa_unprovable")
		})
	}
}

// TestConcurrentRetirementsKeepOneProvider: two retirements at once cannot
// each leave the other's provider as the last and retire both.
func TestConcurrentRetirementsKeepOneProvider(t *testing.T) {
	r := newRegistry(t)
	a, b := r.mustRegister(issuer("a"), "erp"), r.mustRegister(issuer("b"), "erp")
	retire := func(id uuid.UUID) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := identity.RetireIdentityProvider(ctx, tx, r.actor, id, etag.IfMatch("*"))
			return err
		}
	}
	wantProblem(t, interleave(t, r, retire(a.ID), retire(b.ID)), "identity.last_provider")
}

// TestActiveIdentityProviders: the cell lists every tenant's active
// providers as the control role, for its claims in the global directory.
func TestActiveIdentityProviders(t *testing.T) {
	r := newRegistry(t)
	a := r.mustRegister(issuer("active"), "erp")
	b := r.mustRegister(issuer("retired"), "erp")
	if _, err := r.retire(b.ID, "*"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := r.pool.Begin(ctx, database.Scope{Role: "erp_control", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	keys, err := identity.ActiveIdentityProviders(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	want := identity.ProviderKey{ID: a.ID, TenantID: r.tenant, Issuer: a.Issuer, Audience: "erp"}
	if !slices.Contains(keys, want) {
		t.Errorf("active providers: got %d without %+v, want it listed", len(keys), want)
	}
	if slices.ContainsFunc(keys, func(k identity.ProviderKey) bool { return k.Issuer == b.Issuer }) {
		t.Errorf("active providers: got the retired %s, want it left out", b.Issuer)
	}
	if _, err := identity.ActiveIdentityProviders(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("failing read: got %v, want the injected failure", err)
	}
}

// TestRegistry: the cell resolves a token's tenant from its registered
// identity providers, as the control role (ADR-0031).
func TestRegistry(t *testing.T) {
	r := newRegistry(t)
	claim := "erp_tenant"
	p, err := r.register(identity.NewIdentityProvider{Issuer: issuer("registry"), Audience: "erp", TenantClaim: &claim})
	if err != nil {
		t.Fatal(err)
	}
	reg := identity.Registry{Pool: r.pool, ControlRole: "erp_control"}
	ctx := context.Background()
	got, err := reg.Resolve(ctx, p.Issuer, []string{"erp"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderID != p.ID || got.TenantID != r.tenant || got.Issuer != p.Issuer || got.Audience != "erp" || got.TenantClaim == nil || *got.TenantClaim != claim {
		t.Errorf("registration: got %+v, want provider %s of tenant %s with claim %s", got, p.ID, r.tenant, claim)
	}
	if _, err := reg.Resolve(ctx, issuer("nobody"), []string{"erp"}); !errors.Is(err, authn.ErrUnknownIssuer) {
		t.Errorf("unknown: got %v, want ErrUnknownIssuer", err)
	}
	other := newRegistry(t)
	other.mustRegister(p.Issuer, "erp-other")
	if _, err := reg.Resolve(ctx, p.Issuer, []string{"erp", "erp-other"}); !errors.Is(err, authn.ErrAmbiguousIssuer) {
		t.Errorf("ambiguous: got %v, want ErrAmbiguousIssuer", err)
	}
	failing := identity.Registry{Pool: r.pool, ControlRole: "erp_control", Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := r.pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}}
	if _, err := failing.Resolve(ctx, p.Issuer, []string{"erp"}); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("a failing read: got %v, want the injected failure", err)
	}
	down := identity.Registry{Pool: r.pool, ControlRole: "erp_control", Begin: func(context.Context, database.Scope) (pgx.Tx, error) {
		return nil, errors.New("database down")
	}}
	if _, err := down.Resolve(ctx, p.Issuer, []string{"erp"}); err == nil || errors.Is(err, authn.ErrUnknownIssuer) {
		t.Errorf("no transaction: got %v, want a failure to answer", err)
	}
}
