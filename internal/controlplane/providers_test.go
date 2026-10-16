package controlplane_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// providerBody is a registration of a fresh issuer.
func providerBody(name string) (body, issuer string) {
	issuer = "https://" + name + "-" + uuid.NewString() + ".example.com"
	return `{"issuer": "` + issuer + `", "audience": "erp-api", "amr_values": ["mfa"]}`, issuer
}

// auditAction reads the last audit entry of the tenant with the action: its
// actor kind and the operator it names.
func auditAction(t *testing.T, tenant uuid.UUID, action string) (kind, operator string) {
	t.Helper()
	pgtest.Scan(t, `SELECT actor_kind, coalesce(detail->>'operator', '') FROM audit.audit_event WHERE tenant_id = $1 AND action = $2 ORDER BY seq DESC LIMIT 1`,
		[]any{tenant, action}, &kind, &operator)
	return kind, operator
}

func TestIdentityProvidersThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	base := tenants + "/" + ten.ID.String()
	providers := base + "/identity-providers"
	body, iss := providerBody("acme")
	key := "key-" + uuid.NewString()
	created, h := p.must(201, "ops|alice", "POST", providers, body, "Idempotency-Key", key)
	id := created["id"].(string)

	t.Run("registered", func(t *testing.T) {
		if created["issuer"] != iss || created["audience"] != "erp-api" || created["status"] != "active" || created["tenant_claim"] != nil {
			t.Errorf("provider: got %v, want %s for erp-api, active, no tenant claim", created, iss)
		}
		if h.Get("ETag") != `"1"` || h.Get("Location") != providers+"/"+id {
			t.Errorf("headers: got ETag %s, Location %s, want \"1\", %s/%s", h.Get("ETag"), h.Get("Location"), providers, id)
		}
		if kind, op := auditAction(t, ten.ID, "identity.identity_provider.register"); kind != reqctx.KindSystem || op != "ops|alice" {
			t.Errorf("audit: got %s by %s, want system by ops|alice", kind, op)
		}
		events := apitest.Published(t, ten, outbox.Event{Context: "identity", Name: "identity_provider_registered"})
		if len(events) != 1 || events[0]["issuer"] != iss {
			t.Errorf("events: got %v, want one for %s", events, iss)
		}
	})
	t.Run("a retry replays it", func(t *testing.T) {
		again, h := p.in(t).must(201, "ops|alice", "POST", providers, body, "Idempotency-Key", key)
		if again["id"] != id || h.Get("Idempotent-Replayed") != "true" {
			t.Errorf("replay: got %v (replayed %q), want %s replayed", again["id"], h.Get("Idempotent-Replayed"), id)
		}
		p.in(t).problem(422, "request.idempotency_key_reused", "ops|alice", "POST", providers, `{"issuer": "https://other.example.com", "audience": "erp-api"}`, "Idempotency-Key", key)
		p.in(t).problem(422, "request.idempotency_key_reused", "ops|bob", "POST", providers, body, "Idempotency-Key", key)
	})
	t.Run("read", func(t *testing.T) {
		got, h := p.in(t).must(200, "ops|alice", "GET", providers+"/"+id, "")
		if got["id"] != id || h.Get("ETag") != `"1"` {
			t.Errorf("get: got %v, ETag %s", got, h.Get("ETag"))
		}
		list, _ := p.in(t).must(200, "ops|alice", "GET", providers, "")
		if items, _ := list["items"].([]any); len(items) != 1 {
			t.Errorf("list: got %v, want one provider", list)
		}
	})
	t.Run("changed", func(t *testing.T) {
		got, h := p.in(t).must(200, "ops|alice", "PATCH", providers+"/"+id, `{"tenant_claim": "tid", "acr_values": ["urn:loa:2"]}`, "If-Match", `"1"`)
		if got["tenant_claim"] != "tid" || h.Get("ETag") != `"2"` {
			t.Errorf("patch: got %v, ETag %s, want tenant claim tid, \"2\"", got, h.Get("ETag"))
		}
		got, _ = p.in(t).must(200, "ops|alice", "PATCH", providers+"/"+id, `{"tenant_claim": null, "amr_values": null}`, "If-Match", `"2"`)
		if got["tenant_claim"] != nil || len(got["amr_values"].([]any)) != 0 {
			t.Errorf("patch with nulls: got %v, want no tenant claim, no amr values", got)
		}
		if _, op := auditAction(t, ten.ID, "identity.identity_provider.update"); op != "ops|alice" {
			t.Errorf("audit: got operator %q, want ops|alice", op)
		}
		p.in(t).problem(412, "request.version_mismatch", "ops|alice", "PATCH", providers+"/"+id, `{}`, "If-Match", `"1"`)
	})
	t.Run("retired", func(t *testing.T) {
		p.in(t).problem(409, "identity.last_provider", "ops|alice", "POST", providers+"/"+id+":retire", "", "If-Match", "*")
		second, _ := providerBody("second")
		p.in(t).must(201, "ops|alice", "POST", providers, second, "Idempotency-Key", "key-"+uuid.NewString())
		got, _ := p.in(t).must(200, "ops|alice", "POST", providers+"/"+id+":retire", "", "If-Match", "*")
		if got["status"] != "retired" || got["retired_at"] == nil {
			t.Errorf("retired: got %v", got)
		}
		if events := apitest.Published(t, ten, outbox.Event{Context: "identity", Name: "identity_provider_retired"}); len(events) != 1 {
			t.Errorf("events: got %v, want one", events)
		}
		p.in(t).problem(409, "identity.provider_retired", "ops|alice", "POST", providers+"/"+id+":retire", "", "If-Match", "*")
	})
}

func TestAuthPolicyThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	policy := tenants + "/" + ten.ID.String() + "/auth-policy"
	got, h := p.must(200, "ops|alice", "GET", policy, "")
	if got["mfa_required"] != false || got["session_max_age_seconds"] != float64(43200) || got["idle_timeout_seconds"] != float64(1800) || h.Get("ETag") != `"1"` {
		t.Errorf("defaults: got %v, ETag %s", got, h.Get("ETag"))
	}
	p.problem(422, "identity.invalid_policy", "ops|alice", "PATCH", policy, `{"idle_timeout_seconds": 60}`, "If-Match", "*")
	got, h = p.must(200, "ops|alice", "PATCH", policy, `{"mfa_required": true, "idle_timeout_seconds": 900}`, "If-Match", `"1"`)
	if got["mfa_required"] != true || got["idle_timeout_seconds"] != float64(900) || h.Get("ETag") != `"2"` {
		t.Errorf("changed: got %v, ETag %s", got, h.Get("ETag"))
	}
	events := apitest.Published(t, ten, outbox.Event{Context: "identity", Name: "auth_policy_changed"})
	if len(events) != 1 || events[0]["mfa_required"] != true {
		t.Errorf("events: got %v, want one requiring multi-factor", events)
	}
	if _, op := auditAction(t, ten.ID, "identity.auth_policy.update"); op != "ops|alice" {
		t.Errorf("audit: got operator %q, want ops|alice", op)
	}
	body, _ := providerBody("plain")
	p.problem(422, "identity.mfa_unprovable", "ops|alice", "POST", tenants+"/"+ten.ID.String()+"/identity-providers",
		strings.Replace(body, `, "amr_values": ["mfa"]`, "", 1), "Idempotency-Key", "key-"+uuid.NewString())
}

func TestIdentityRequestsRefused(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	base := tenants + "/" + ten.ID.String()
	unknown := tenants + "/" + uuid.NewString()
	body, _ := providerBody("refused")
	key := func() string { return "key-" + uuid.NewString() }
	tooLarge := `{"acr_values": ["` + strings.Repeat("a", 5<<20) + `"]}`
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		code                     string
		extra                    []string
	}{
		{"unknown tenant's providers", "GET", unknown + "/identity-providers", "", 404, "request.not_found", nil},
		{"unknown tenant's registration", "POST", unknown + "/identity-providers", body, 404, "request.not_found", []string{"Idempotency-Key", key()}},
		{"unknown tenant's policy", "GET", unknown + "/auth-policy", "", 404, "request.not_found", nil},
		{"unknown tenant's policy change", "PATCH", unknown + "/auth-policy", `{}`, 404, "request.not_found", []string{"If-Match", "*"}},
		{"unknown provider", "GET", base + "/identity-providers/" + uuid.NewString(), "", 404, "request.not_found", nil},
		{"provider id not a uuid", "GET", base + "/identity-providers/nope", "", 400, "request.invalid_parameter", nil},
		{"tenant id not a uuid", "GET", tenants + "/nope/identity-providers", "", 400, "request.invalid_parameter", nil},
		{"no Idempotency-Key", "POST", base + "/identity-providers", body, 428, "request.precondition_required", nil},
		{"no If-Match", "PATCH", base + "/auth-policy", `{}`, 428, "request.precondition_required", nil},
		{"not JSON", "POST", base + "/identity-providers", `{`, 400, "request.invalid_body", []string{"Idempotency-Key", key()}},
		{"an unknown field", "POST", base + "/identity-providers", `{"issuer": "https://x.example.com", "audience": "a", "colour": "red"}`, 400, "request.invalid_body", []string{"Idempotency-Key", key()}},
		{"a provider patch not JSON", "PATCH", base + "/identity-providers/" + uuid.NewString(), `[`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a provider patch with an issuer", "PATCH", base + "/identity-providers/" + uuid.NewString(), `{"issuer": "https://x.example.com"}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a provider patch with a number", "PATCH", base + "/identity-providers/" + uuid.NewString(), `{"acr_values": 7}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a provider patch with a numeric claim", "PATCH", base + "/identity-providers/" + uuid.NewString(), `{"tenant_claim": 5}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a provider patch too large", "PATCH", base + "/identity-providers/" + uuid.NewString(), tooLarge, 413, "request.body_too_large", []string{"If-Match", "*"}},
		{"a policy patch too large", "PATCH", base + "/auth-policy", tooLarge, 413, "request.body_too_large", []string{"If-Match", "*"}},
		{"a policy patch with a string", "PATCH", base + "/auth-policy", `{"mfa_required": "yes"}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"an invalid provider", "POST", base + "/identity-providers", `{"issuer": "http://idp.example.com", "audience": "erp"}`, 422, "identity.invalid_provider", []string{"Idempotency-Key", key()}},
		{"an unknown method", "POST", base + "/identity-providers/" + uuid.NewString() + ":promote", "", 404, "request.not_found", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.in(t).problem(c.status, c.code, "ops|alice", c.method, c.path, c.body, c.extra...)
		})
	}
	for _, c := range []struct{ method, path, allow string }{
		{"DELETE", base + "/identity-providers", "GET, POST"},
		{"PUT", base + "/identity-providers/" + uuid.NewString(), "GET, PATCH"},
		{"GET", base + "/identity-providers/" + uuid.NewString() + ":retire", "POST"},
		{"POST", base + "/auth-policy", "GET, PATCH"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			status, _, h := p.in(t).as("ops|alice", c.method, c.path, "")
			if status != 405 || h.Get("Allow") != c.allow {
				t.Errorf("got %d Allow %q, want 405 Allow %q", status, h.Get("Allow"), c.allow)
			}
		})
	}
}

// TestIdentityOperationsNeedTheirRoles: reading needs erp:read; changing a
// tenant's identity providers or policy needs erp:admin (ADR-0027 as
// amended by ADR-0031), not erp:operate.
func TestIdentityOperationsNeedTheirRoles(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	providers := tenants + "/" + ten.ID.String() + "/identity-providers"
	with := func(roles ...any) string {
		return "Bearer " + p.is.Token(p.t, uuid.Nil, "ops|roled", map[string]any{"tenant_id": nil, "roles": roles})
	}
	body, _ := providerBody("roles")
	for _, c := range []struct {
		name, method, path, body, auth string
		code                           int
	}{
		{"reader lists", "GET", providers, "", with("erp:read"), 200},
		{"reader reads the policy", "GET", tenants + "/" + ten.ID.String() + "/auth-policy", "", with("erp:read"), 200},
		{"reader registers", "POST", providers, body, with("erp:read"), 403},
		{"operator registers", "POST", providers, body, with("erp:operate"), 403},
		{"operator changes the policy", "PATCH", tenants + "/" + ten.ID.String() + "/auth-policy", `{}`, with("erp:operate"), 403},
		{"admin registers", "POST", providers, body, with("erp:admin"), 201},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, m, _ := p.in(t).do(c.method, c.path, c.body, map[string]string{"Authorization": c.auth, "If-Match": "*", "Idempotency-Key": "key-" + uuid.NewString()})
			if status != c.code {
				t.Errorf("status: got %d, want %d: %v", status, c.code, m)
			}
		})
	}
}

// TestRegistrationInProduction: in production an issuer is https; http on
// loopback is for a provider on a developer's machine.
func TestRegistrationInProduction(t *testing.T) {
	s := service(t)
	s.Production = true
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	_, _, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: "http://localhost:8080/realms/x", Audience: "erp"}, nil)
	pgtest.CheckErr(t, err, "identity.invalid_provider")
}

// TestRegistrationClaimsTheIssuer: with a global directory the issuer is
// claimed before the registration commits, so another cell's tenant cannot
// register it; retiring gives the claim up.
func TestRegistrationClaimsTheIssuer(t *testing.T) {
	ctx := context.Background()
	s := service(t)
	dir, err := directory.Open(ctx, pgtest.EmptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	s.Directory = &directory.Sync{Pool: s.Pool, ControlRole: "erp_control", Directory: dir, Cell: "claim-cell", APIURL: "https://claim.example.com"}
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	in := identity.NewIdentityProvider{Issuer: "https://claimed-" + uuid.NewString() + ".example.com", Audience: "erp"}
	if err := dir.ClaimIssuer(ctx, directory.Issuer{Issuer: in.Issuer, Audience: in.Audience, TenantID: uuid.New(), Cell: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.RegisterProvider(ctx, op, ten.ID, in, nil)
	pgtest.CheckErr(t, err, "identity.provider_taken")
	if _, err := dir.SyncIssuers(ctx, "elsewhere", nil); err != nil { // the other cell gives the pair up
		t.Fatal(err)
	}
	first, _, err := s.RegisterProvider(ctx, op, ten.ID, in, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := s.RegisterProvider(ctx, op, ten.ID, identity.NewIdentityProvider{Issuer: "https://spare-" + uuid.NewString() + ".example.com", Audience: "erp"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireProvider(ctx, op, ten.ID, first.ID, "*"); err != nil {
		t.Fatal(err)
	}
	if err := dir.ClaimIssuer(ctx, directory.Issuer{Issuer: in.Issuer, Audience: in.Audience, TenantID: uuid.New(), Cell: "elsewhere"}); err != nil {
		t.Errorf("after the retirement another tenant claims it: got %v, want claimed", err)
	}
}

// TestIdentityDatabaseFailures: every statement of every identity
// operation failing in turn is reported, and a registration whose commit
// fails stores no key to replay.
func TestIdentityDatabaseFailures(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	ten := pgtest.NewTenant(t)
	in := func() identity.NewIdentityProvider {
		return identity.NewIdentityProvider{Issuer: "https://f-" + uuid.NewString() + ".example.com", Audience: "erp", AMRValues: []string{"mfa"}}
	}
	first, _, err := s.RegisterProvider(ctx, op, ten.ID, in(), nil)
	if err != nil {
		t.Fatal(err)
	}
	replayKey := &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")}
	if _, _, err := s.RegisterProvider(ctx, op, ten.ID, in(), replayKey); err != nil {
		t.Fatal(err)
	}
	yes := true
	for name, fn := range map[string]func(ctx context.Context) error{
		"list":   func(ctx context.Context) error { _, err := s.ListProviders(ctx, ten.ID); return err },
		"get":    func(ctx context.Context) error { _, err := s.GetProvider(ctx, ten.ID, first.ID); return err },
		"policy": func(ctx context.Context) error { _, err := s.AuthPolicy(ctx, ten.ID); return err },
		"replay": func(ctx context.Context) error {
			_, _, err := s.RegisterProvider(ctx, op, ten.ID, in(), replayKey)
			return err
		},
		"register": func(ctx context.Context) error {
			_, _, err := s.RegisterProvider(ctx, op, ten.ID, in(), &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := s.UpdateProvider(ctx, op, ten.ID, first.ID, "*", identity.IdentityProviderChange{ACRValues: &[]string{"x"}})
			return err
		},
		"change policy": func(ctx context.Context) error {
			_, err := s.UpdateAuthPolicy(ctx, op, ten.ID, "*", identity.AuthPolicyChange{MFARequired: &yes})
			return err
		},
		"retire": func(ctx context.Context) error {
			spare, _, err := s.RegisterProvider(context.Background(), op, ten.ID, in(), nil) // outside the failing transactions
			if err != nil {
				return err
			}
			_, err = s.RetireProvider(ctx, op, ten.ID, spare.ID, "*")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			everyFailure(t, s, name, fn)
		})
	}
	s.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	_, err = s.ListProviders(ctx, ten.ID)
	pgtest.CheckErr(t, err, "database down")
	_, _, err = s.RegisterProvider(ctx, op, ten.ID, in(), nil)
	pgtest.CheckErr(t, err, "database down")
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	lost := &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")}
	if _, _, err := s.RegisterProvider(ctx, op, ten.ID, in(), lost); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("commit: got %v, want the injected failure", err)
	}
	s.Begin = nil
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND key = $2`, []any{ten.ID, "control|" + lost.Key}, &n)
	if n != 0 {
		t.Errorf("keys stored by the failed commit: got %d, want 0", n)
	}
}
