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
