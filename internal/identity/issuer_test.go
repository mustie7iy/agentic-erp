package identity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestCreatorWithoutIssuer: a new principal's subject is bound to the
// issuer of the principal creating it (ADR-0010). An actor bound to no
// issuer (an agent, a job's actor, a principal the previous release
// created during the issuer rollout) cannot give one, so creating a person
// or service is refused and writes nothing, rather than leaving a
// principal no token can ever sign in as. An agent, which has no subject,
// needs no issuer.
func TestCreatorWithoutIssuer(t *testing.T) {
	ten := pgtest.NewTenant(t)
	for _, kind := range []string{"agent", "human"} {
		t.Run(kind, func(t *testing.T) {
			actor := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: kind}
			// The person has a subject but no issuer, as the previous
			// release inserts them during the rollout (migration 000037).
			var own *string
			if kind == "human" {
				own = new("previous|" + actor.ID.String())
			}
			pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by)
				VALUES ($1, $2, $3, $4, 'No issuer', 'active', $2, $2)`, ten.ID, actor.ID, kind, own)
			subject := "auth0|" + actor.ID.String()
			err := apitest.Committed(t, ten, actor, func(ctx context.Context) error {
				_, err := identity.CreatePrincipal(ctx, identity.NewPrincipal{Kind: identity.Human, ExternalSubject: &subject, DisplayName: "Locked out"})
				return err
			})
			if pr := problem.From(err); pr.Code != "identity.creator_without_issuer" {
				t.Errorf("person created by an actor without an issuer: got %v, want identity.creator_without_issuer", err)
			}
			var n int
			pgtest.Scan(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1 AND external_subject = $2`, []any{ten.ID, subject}, &n)
			if n != 0 {
				t.Errorf("principals written: got %d, want 0", n)
			}
			err = apitest.Committed(t, ten, actor, func(ctx context.Context) error {
				_, err := identity.CreatePrincipal(ctx, identity.NewPrincipal{Kind: identity.Agent, DisplayName: "Helper",
					Profile: &identity.ProfileInput{Model: "m", Description: "d"}})
				return err
			})
			if err != nil {
				t.Errorf("agent created by an actor without an issuer: got %v, want created", err)
			}
		})
	}
}

// TestFirstAdministratorWithoutIssuer: an administrator given no issuer
// would match no token and leave the tenant without anyone to administer
// it, so the caller's wiring mistake is refused before anything is written.
func TestFirstAdministratorWithoutIssuer(t *testing.T) {
	err := identity.CreateFirstAdministrator(context.Background(), nil, identity.FirstAdministrator{ID: uuid.Must(uuid.NewV7()), Subject: "auth0|admin"})
	if err == nil {
		t.Fatal("administrator without an issuer: got created, want refused")
	}
	if !strings.Contains(err.Error(), "no issuer") {
		t.Errorf("administrator without an issuer: got %v, want an error naming the missing issuer", err)
	}
}

// TestIssuerFollowsTheSubject: the API reports a principal's issuer
// whenever it has a subject, whatever its kind (T30): an agent created
// with a subject is bound like a person; an agent without one has none,
// published as null; and a person the previous release created during the
// issuer rollout, still unbound, is read back without one.
func TestIssuerFollowsTheSubject(t *testing.T) {
	c, ten := admin(t)
	profile := `"agent_profile": {"autonomy_level": 0, "model": "m", "description": "d"}`
	bound, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "agent", "external_subject": "bot|ap", "display_name": "Bot", `+profile+`}`, "")
	plain, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "agent", "display_name": "Plain", `+profile+`}`, "")
	unbound := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', 'previous|unbound', 'Unbound', 'active', $2, $2)`, ten.ID, unbound)
	read, _ := c.Must(200, "GET", "/v1/principals/"+unbound.String(), "", "")

	t.Run("agent with a subject", func(t *testing.T) {
		if got, want := bound["external_issuer"], any(c.API.Issuer.URL); got != want {
			t.Errorf("external_issuer: got %v, want %v", got, want)
		}
	})
	t.Run("agent without a subject", func(t *testing.T) {
		if got, present := plain["external_issuer"]; present {
			t.Errorf("external_issuer: got %v, want absent", got)
		}
	})
	t.Run("person not yet bound", func(t *testing.T) {
		if got, present := read["external_issuer"]; present {
			t.Errorf("external_issuer: got %v, want absent", got)
		}
	})
	t.Run("events", func(t *testing.T) {
		created := events(t, ten, "principal_created")
		if len(created) != 2 {
			t.Fatalf("created events: got %d, want 2", len(created))
		}
		if got, want := created[0]["external_issuer"], any(c.API.Issuer.URL); got != want {
			t.Errorf("agent with a subject: event external_issuer: got %v, want %v", got, want)
		}
		if got, present := created[1]["external_issuer"]; !present || got != nil {
			t.Errorf("agent without a subject: event external_issuer: got %v (present %t), want null", got, present)
		}
	})
}
