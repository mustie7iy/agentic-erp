package controlplane_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// mergePatch is the media type of the contract's PATCH bodies (RFC 7396).
const mergePatch = "application/merge-patch+json"

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
		got, h := p.in(t).must(200, "ops|alice", "PATCH", providers+"/"+id, `{"tenant_claim": "tid", "acr_values": ["urn:loa:2"]}`, "If-Match", `"1"`, "Content-Type", mergePatch)
		if got["tenant_claim"] != "tid" || h.Get("ETag") != `"2"` {
			t.Errorf("patch: got %v, ETag %s, want tenant claim tid, \"2\"", got, h.Get("ETag"))
		}
		got, _ = p.in(t).must(200, "ops|alice", "PATCH", providers+"/"+id, `{"tenant_claim": null, "amr_values": null}`, "If-Match", `"2"`, "Content-Type", mergePatch)
		if got["tenant_claim"] != nil || len(got["amr_values"].([]any)) != 0 {
			t.Errorf("patch with nulls: got %v, want no tenant claim, no amr values", got)
		}
		if _, op := auditAction(t, ten.ID, "identity.identity_provider.update"); op != "ops|alice" {
			t.Errorf("audit: got operator %q, want ops|alice", op)
		}
		p.in(t).problem(412, "request.version_mismatch", "ops|alice", "PATCH", providers+"/"+id, `{}`, "If-Match", `"1"`, "Content-Type", mergePatch)
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
	invalid := p.problem(422, "identity.invalid_policy", "ops|alice", "PATCH", policy, `{"idle_timeout_seconds": 60}`, "If-Match", "*", "Content-Type", mergePatch)
	if errs, _ := invalid["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "idle_timeout_seconds" {
		t.Errorf("field errors: got %v, want one on idle_timeout_seconds, the field the request names", invalid["errors"])
	}
	got, h = p.must(200, "ops|alice", "PATCH", policy, `{"mfa_required": true, "idle_timeout_seconds": 900}`, "If-Match", `"1"`, "Content-Type", mergePatch)
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
		{"a policy patch with a null", "PATCH", base + "/auth-policy", `{"mfa_required": null}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"a policy patch with a string", "PATCH", base + "/auth-policy", `{"mfa_required": "yes"}`, 400, "request.invalid_body", []string{"If-Match", "*"}},
		{"an invalid provider", "POST", base + "/identity-providers", `{"issuer": "http://idp.example.com", "audience": "erp"}`, 422, "identity.invalid_provider", []string{"Idempotency-Key", key()}},
		{"an unknown method", "POST", base + "/identity-providers/" + uuid.NewString() + ":promote", "", 404, "request.not_found", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			extra := c.extra
			if c.method == "PATCH" {
				extra = append(extra, "Content-Type", mergePatch)
			}
			p.in(t).problem(c.status, c.code, "ops|alice", c.method, c.path, c.body, extra...)
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
			status, m, _ := p.in(t).do(c.method, c.path, c.body, map[string]string{"Authorization": c.auth, "If-Match": "*", "Idempotency-Key": "key-" + uuid.NewString(), "Content-Type": mergePatch})
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
	elsewhere := directory.Issuer{Issuer: in.Issuer, Audience: in.Audience, TenantID: uuid.New(), Cell: "elsewhere", ProviderID: uuid.New()}
	if err := dir.ClaimIssuer(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.RegisterProvider(ctx, op, ten.ID, in, nil)
	pgtest.CheckErr(t, err, "identity.provider_taken")
	if err := dir.ReleaseIssuer(ctx, elsewhere); err != nil { // the other cell's tenant retires it
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

// TestPatchesAreMergePatches: the PATCH operations take
// application/merge-patch+json only, as the public API's do.
func TestPatchesAreMergePatches(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	base := tenants + "/" + ten.ID.String()
	for _, path := range []string{base + "/auth-policy", base + "/identity-providers/" + uuid.NewString()} {
		for _, ct := range []string{"application/json", "", "text/plain"} {
			t.Run(path+" "+ct, func(t *testing.T) {
				p.in(t).problem(400, "request.invalid_body", "ops|alice", "PATCH", path, `{}`, "If-Match", "*", "Content-Type", ct)
			})
		}
	}
	p.must(200, "ops|alice", "PATCH", base+"/auth-policy", `{}`, "If-Match", "*", "Content-Type", mergePatch+"; charset=utf-8")
}

// TestIdentityWritesOnOffboardedTenants: a suspended tenant's registry and
// policy can still be changed; once offboarding has started they cannot
// (409 tenancy.invalid_transition), and they can still be read.
func TestIdentityWritesOnOffboardedTenants(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	base := tenants + "/" + ten.ID.String()
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'suspended', version = version + 1 WHERE id = $1`, ten.ID)
	body, _ := providerBody("suspended")
	created, _ := p.must(201, "ops|alice", "POST", base+"/identity-providers", body, "Idempotency-Key", "key-"+uuid.NewString())
	provider := base + "/identity-providers/" + created["id"].(string)
	p.must(200, "ops|alice", "PATCH", base+"/auth-policy", `{"idle_timeout_seconds": 600}`, "If-Match", "*", "Content-Type", mergePatch)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, ten.ID)
	again, _ := providerBody("offboarded")
	for _, c := range []struct{ name, method, path, body string }{
		{"register", "POST", base + "/identity-providers", again},
		{"update", "PATCH", provider, `{"acr_values": ["x"]}`},
		{"retire", "POST", provider + ":retire", ""},
		{"change the policy", "PATCH", base + "/auth-policy", `{"idle_timeout_seconds": 900}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.in(t).problem(409, "tenancy.invalid_transition", "ops|alice", c.method, c.path, c.body,
				"If-Match", "*", "Idempotency-Key", "key-"+uuid.NewString(), "Content-Type", mergePatch)
		})
	}
	p.must(200, "ops|alice", "GET", provider, "")
	p.must(200, "ops|alice", "GET", base+"/auth-policy", "")
}

// TestRegistrationKeysExpire: a key past its expiry is not replayed, as
// for provisioning; the registration runs afresh.
func TestRegistrationKeysExpire(t *testing.T) {
	s := service(t)
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	key := &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")}
	stale := uuid.New()
	pgtest.Exec(t, `INSERT INTO platform.idempotency_key (tenant_id, key, principal_id, request_hash, status, response_code, response_body, expires_at)
		VALUES ($1, $2, $3, $4, 'completed', 201, $5, now() - interval '1 minute')`, ten.ID, provisioning.KeyPrefix+key.Key, op.ID, key.RequestHash, `{"id": "`+stale.String()+`"}`)
	got, replayed, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: "https://expired-" + uuid.NewString() + ".example.com", Audience: "erp"}, key)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || got.ID == stale {
		t.Errorf("an expired key: got %s (replayed %v), want a new registration", got.ID, replayed)
	}
}

// pausedCommit is a transaction whose commit waits until release is
// closed, after announcing it on reached.
type pausedCommit struct {
	pgx.Tx
	reached chan<- int32
	release <-chan struct{}
	pid     int32
}

func (p *pausedCommit) Commit(ctx context.Context) error {
	p.reached <- p.pid
	<-p.release
	return p.Tx.Commit(ctx)
}

// TestConcurrentRegistrationsWithOneKey: two registrations with the same
// key at once register once; the second waits for the first and replays
// it, rather than answering that the issuer is taken.
// The second waits on the tenant's row, which every registry write holds
// (inTenant); the key claimed first (claimKey) is what makes it replay
// rather than register again once the first has committed.
func TestConcurrentRegistrationsWithOneKey(t *testing.T) {
	s := service(t)
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	key := &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")}
	in := identity.NewIdentityProvider{Issuer: "https://race-" + uuid.NewString() + ".example.com", Audience: "erp"}
	reached, release := make(chan int32, 1), make(chan struct{})
	var once atomic.Bool
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, scope)
		if err != nil || !once.CompareAndSwap(false, true) {
			return tx, err
		}
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return nil, err
		}
		return &pausedCommit{Tx: tx, reached: reached, release: release, pid: pid}, nil
	}
	type result struct {
		p        controlplane.IdentityProvider
		replayed bool
		err      error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	ctx := context.Background()
	go func() {
		p, r, err := s.RegisterProvider(ctx, op, ten.ID, in, key)
		first <- result{p, r, err}
	}()
	pid := <-reached
	go func() {
		p, r, err := s.RegisterProvider(ctx, op, ten.ID, in, key)
		second <- result{p, r, err}
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}, "the second registration does not wait for the first")
	close(release)
	a, b := <-first, <-second
	if a.err != nil || a.replayed {
		t.Fatalf("first: got %v (replayed %v), want registered", a.err, a.replayed)
	}
	if b.err != nil || !b.replayed || b.p.ID != a.p.ID {
		t.Errorf("second: got %s, %v (replayed %v), want %s replayed", b.p.ID, b.err, b.replayed, a.p.ID)
	}
}

// recordingTx records the statements it runs.
type recordingTx struct {
	pgx.Tx
	mu   *sync.Mutex
	seen *[]string
}

func (r recordingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	*r.seen = append(*r.seen, sql)
	r.mu.Unlock()
	return r.Tx.Exec(ctx, sql, args...)
}

// TestRegistrationsAreBoundedInTime: a registration's transaction has a
// transaction timeout, so its directory claim, made before the commit, is
// never older than the grace a round gives a claim (directory.ClaimGrace).
func TestRegistrationsAreBoundedInTime(t *testing.T) {
	s := service(t)
	ten := pgtest.NewTenant(t)
	var mu sync.Mutex
	var seen []string
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, scope)
		return recordingTx{Tx: tx, mu: &mu, seen: &seen}, err
	}
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	if _, _, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: "https://timed-" + uuid.NewString() + ".example.com", Audience: "erp"}, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(seen, func(sql string) bool { return strings.Contains(sql, "transaction_timeout") }) {
		t.Errorf("statements: got %q, want a transaction_timeout set", seen)
	}
	if controlplane.DefaultWriteTimeout >= directory.ClaimGrace {
		t.Errorf("write timeout %v: want it below the claim grace %v", controlplane.DefaultWriteTimeout, directory.ClaimGrace)
	}
}

// sleepyCommit sleeps in the transaction before committing it.
type sleepyCommit struct{ pgx.Tx }

func (s sleepyCommit) Commit(ctx context.Context) error {
	if _, err := s.Exec(ctx, `SELECT pg_sleep(2)`); err != nil {
		return err
	}
	return s.Tx.Commit(ctx)
}

// TestWriteTimeoutEndsALongRegistration: a registration whose transaction
// outlives the write timeout is ended by the database and commits nothing,
// so its claim in the directory is given up by the next round past the
// grace, never kept for a provider that does not exist.
func TestWriteTimeoutEndsALongRegistration(t *testing.T) {
	s := service(t)
	s.WriteTimeout = 300 * time.Millisecond
	s.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := s.Pool.Begin(ctx, scope)
		return sleepyCommit{tx}, err
	}
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	in := identity.NewIdentityProvider{Issuer: "https://slow-" + uuid.NewString() + ".example.com", Audience: "erp"}
	_, _, err := s.RegisterProvider(context.Background(), op, ten.ID, in, nil)
	if err == nil || !strings.Contains(err.Error(), "transaction timeout") {
		t.Fatalf("a registration outliving the write timeout: got %v, want it ended by the transaction timeout", err)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM identity.identity_provider WHERE issuer = $1`, []any{in.Issuer}, &n)
	if n != 0 {
		t.Errorf("providers registered: got %d, want 0", n)
	}
}

// TestRegistrationProbesTheProvider: a provider whose discovery document
// cannot be read is refused at registration, 422
// identity.provider_unreachable, for registration and provisioning alike.
func TestRegistrationProbesTheProvider(t *testing.T) {
	s := service(t)
	s.Discover = func(_ context.Context, issuer string) error {
		if strings.Contains(issuer, "down") {
			return errors.New("connection refused")
		}
		return nil
	}
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	_, _, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: "https://down-" + uuid.NewString() + ".example.com", Audience: "erp"}, nil)
	pgtest.CheckErr(t, err, "identity.provider_unreachable")
	if _, _, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: "https://up-" + uuid.NewString() + ".example.com", Audience: "erp"}, nil); err != nil {
		t.Errorf("a provider that answers: got %v, want registered", err)
	}
}

// TestProvisioningWithAProviderThroughTheControlPlane: createTenant takes
// the tenant's identity provider and answers its id.
func TestProvisioningWithAProviderThroughTheControlPlane(t *testing.T) {
	p := newPlane(t)
	body := strings.TrimSuffix(strings.TrimSpace(tenantBody(t, "With a provider")), "}") +
		`, "identity_provider": {"issuer": "https://idp-` + uuid.NewString() + `.example.com", "audience": "erp-api", "amr_values": ["mfa"]}}`
	created, _ := p.must(201, "ops|alice", "POST", tenants, body, "Idempotency-Key", "key-"+uuid.NewString())
	provisioned, _ := created["provisioned"].(map[string]any)
	if provisioned["identity_provider_id"] == nil {
		t.Fatalf("provisioned: got %v, want the identity provider's id", provisioned)
	}
	list, _ := p.must(200, "ops|alice", "GET", tenants+"/"+created["id"].(string)+"/identity-providers", "")
	if items, _ := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != provisioned["identity_provider_id"] {
		t.Errorf("providers: got %v, want the one provisioned", list)
	}
	p.problem(400, "request.invalid_body", "ops|alice", "POST", tenants, strings.Replace(body, `"audience"`, `"colour": "red", "audience"`, 1), "Idempotency-Key", "key-"+uuid.NewString())
}

// TestBindingUnboundPrincipals: an operator lists a tenant's principals
// whose issuer is no active provider of the tenant (such as the
// development issuer 000046 bound them to) and binds one to a registered
// provider with its subject there, so it signs in again (ADR-0031).
func TestBindingUnboundPrincipals(t *testing.T) {
	p := newPlane(t)
	ten := pgtest.NewTenant(t)
	base := tenants + "/" + ten.ID.String()
	pgtest.Exec(t, `UPDATE identity.principal SET external_issuer = 'erp-dev', version = version + 1 WHERE tenant_id = $1 AND id = $2`, ten.ID, ten.Human.ID)
	body, iss := providerBody("bind")
	provider, _ := p.must(201, "ops|alice", "POST", base+"/identity-providers", body, "Idempotency-Key", "key-"+uuid.NewString())

	unbound, _ := p.must(200, "ops|alice", "GET", base+"/principals", "")
	items, _ := unbound["items"].([]any)
	ids := map[any]bool{}
	for _, it := range items {
		ids[it.(map[string]any)["id"]] = true
	}
	// The service and agent are bound to the test issuer, which is no provider of the tenant: unbound too.
	if !ids[ten.Human.ID.String()] || len(items) != 3 {
		t.Fatalf("unbound principals: got %v, want the three, the person among them", unbound)
	}
	all, _ := p.must(200, "ops|alice", "GET", base+"/principals?binding=all&page_size=2", "")
	if items, _ := all["items"].([]any); len(items) != 2 || all["next_page_token"] == nil {
		t.Errorf("a page of all principals: got %v, want two and a next page", all)
	}
	p.problem(400, "request.invalid_parameter", "ops|alice", "GET", base+"/principals?binding=some", "")

	path := base + "/principals/" + ten.Human.ID.String() + ":bind"
	bind := `{"identity_provider_id": "` + provider["id"].(string) + `", "subject": "ada@acme"}`
	got, h := p.must(200, "ops|alice", "POST", path, bind, "If-Match", `"2"`)
	if got["external_issuer"] != iss || got["external_subject"] != "ada@acme" || got["bound"] != true || h.Get("ETag") != `"3"` {
		t.Errorf("bound: got %v, ETag %s, want %s's ada@acme, bound, \"3\"", got, h.Get("ETag"), iss)
	}
	events := apitest.Published(t, ten, outbox.Event{Context: "identity", Name: "principal_bound"})
	if len(events) != 1 || events[0]["previous_issuer"] != "erp-dev" || events[0]["external_issuer"] != iss {
		t.Errorf("events: got %v, want one from erp-dev to %s", events, iss)
	}
	if _, op := auditAction(t, ten.ID, "identity.principal.bind"); op != "ops|alice" {
		t.Errorf("audit: got operator %q, want ops|alice", op)
	}
	after, _ := p.must(200, "ops|alice", "GET", base+"/principals", "")
	if items, _ := after["items"].([]any); len(items) != 2 {
		t.Errorf("unbound after the binding: got %v, want the two machines", after)
	}

	t.Run("refusals", func(t *testing.T) {
		pr := p.in(t)
		pr.problem(412, "request.version_mismatch", "ops|alice", "POST", path, bind, "If-Match", `"2"`)
		pr.problem(409, "identity.subject_taken", "ops|alice", "POST", base+"/principals/"+ten.Service.ID.String()+":bind", bind, "If-Match", "*")
		pr.problem(404, "request.not_found", "ops|alice", "POST", base+"/principals/"+uuid.NewString()+":bind", bind, "If-Match", "*")
		pr.problem(422, "request.reference_not_found", "ops|alice", "POST", path, `{"identity_provider_id": "`+uuid.NewString()+`", "subject": "x"}`, "If-Match", "*")
		pr.problem(422, "identity.invalid_subject", "ops|alice", "POST", path, `{"identity_provider_id": "`+provider["id"].(string)+`", "subject": ""}`, "If-Match", "*")
		pr.problem(400, "request.invalid_body", "ops|alice", "POST", path, `{"subject": "x", "colour": "red"}`, "If-Match", "*")
		pr.problem(400, "request.invalid_parameter", "ops|alice", "POST", base+"/principals/nope:bind", bind, "If-Match", "*")
		pr.problem(400, "request.invalid_parameter", "ops|alice", "GET", base+"/principals?page_size=many", "")
		pr.problem(413, "request.body_too_large", "ops|alice", "POST", path, `{"subject": "`+strings.Repeat("x", 5000)+`"}`, "If-Match", "*")
		pr.problem(404, "request.not_found", "ops|alice", "POST", base+"/principals/"+ten.Human.ID.String()+":promote", bind, "If-Match", "*")
		status, _, hd := pr.as("ops|alice", "GET", path, "")
		if status != 405 || hd.Get("Allow") != "POST" {
			t.Errorf("GET of :bind: got %d Allow %q, want 405 POST", status, hd.Get("Allow"))
		}
		status, _, hd = pr.as("ops|alice", "DELETE", base+"/principals", "")
		if status != 405 || hd.Get("Allow") != "GET" {
			t.Errorf("DELETE of principals: got %d Allow %q, want 405 GET", status, hd.Get("Allow"))
		}
	})
	t.Run("a retired provider", func(t *testing.T) {
		pr := p.in(t)
		second, _ := providerBody("second")
		pr.must(201, "ops|alice", "POST", base+"/identity-providers", second, "Idempotency-Key", "key-"+uuid.NewString())
		pr.must(200, "ops|alice", "POST", base+"/identity-providers/"+provider["id"].(string)+":retire", "", "If-Match", "*")
		pr.problem(422, "identity.provider_retired", "ops|alice", "POST", path, bind, "If-Match", "*")
	})
}

func TestBindingDatabaseFailures(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	ten := pgtest.NewTenant(t)
	prov, _, err := s.RegisterProvider(ctx, op, ten.ID, identity.NewIdentityProvider{Issuer: "https://bind-f-" + uuid.NewString() + ".example.com", Audience: "erp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	everyFailure(t, s, "list", func(ctx context.Context) error {
		_, _, err := s.ListPrincipals(ctx, ten.ID, "unbound", pagination.Request{})
		return err
	})
	everyFailure(t, s, "bind", func(ctx context.Context) error {
		_, err := s.BindPrincipal(ctx, op, ten.ID, ten.Human.ID, "*", prov.ID, "f-"+uuid.NewString())
		return err
	})
	_, _, err = s.ListPrincipals(ctx, ten.ID, "all", pagination.Request{PageToken: "garbage", PageTokenSet: true})
	if got := problem.From(err).Status; got != 400 {
		t.Errorf("a bad page token: got %d (%v), want 400", got, err)
	}
}

// TestAnInvalidIssuerIsNeverProbed: a registration is checked before its
// issuer is fetched, so an issuer no registration could hold (a query, an
// http host off loopback) is never requested from the cell.
func TestAnInvalidIssuerIsNeverProbed(t *testing.T) {
	s := service(t)
	var probed []string
	s.Discover = func(_ context.Context, issuer string) error { probed = append(probed, issuer); return nil }
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	for _, issuer := range []string{"http://169.254.169.254/latest/meta-data/x?a=", "ftp://idp.example.com"} {
		_, _, err := s.RegisterProvider(context.Background(), op, ten.ID, identity.NewIdentityProvider{Issuer: issuer, Audience: "erp"}, nil)
		pgtest.CheckErr(t, err, "identity.invalid_provider")
	}
	if len(probed) != 0 {
		t.Errorf("probed: got %v, want nothing", probed)
	}
}

// TestARetryReplaysWithoutProbing: a registration retried with its key
// replays the first answer even when the provider has stopped answering
// since.
func TestARetryReplaysWithoutProbing(t *testing.T) {
	s := service(t)
	down := false
	s.Discover = func(context.Context, string) error {
		if down {
			return errors.New("connection refused")
		}
		return nil
	}
	ten := pgtest.NewTenant(t)
	op := controlplane.OperatorFrom(reqctx.Claims{Issuer: "https://ops", Subject: "alice"})
	key := &provisioning.Idempotency{Key: "key-" + uuid.NewString(), RequestHash: []byte("h")}
	in := identity.NewIdentityProvider{Issuer: "https://flaps-" + uuid.NewString() + ".example.com", Audience: "erp"}
	first, _, err := s.RegisterProvider(context.Background(), op, ten.ID, in, key)
	if err != nil {
		t.Fatal(err)
	}
	down = true
	again, replayed, err := s.RegisterProvider(context.Background(), op, ten.ID, in, key)
	if err != nil || !replayed || again.ID != first.ID {
		t.Errorf("retry: got %s (replayed %v), %v, want %s replayed", again.ID, replayed, err, first.ID)
	}
}
