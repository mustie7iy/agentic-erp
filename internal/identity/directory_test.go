package identity_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestDirectory(t *testing.T) {
	ten := pgtest.NewTenant(t)
	d := identity.Directory{}
	err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		role, ok, err := d.RoleByCode(ctx, "all")
		if err != nil || !ok {
			t.Fatalf("role: %v %v", ok, err)
		}
		_, ok, err = d.RoleByCode(ctx, "none")
		if err != nil {
			t.Errorf("unknown role: %v", err)
		}
		if ok {
			t.Errorf("unknown role: got found, want not found")
		}
		holds, err := d.HoldsRole(ctx, ten.Human.ID, role)
		if err != nil {
			t.Errorf("human: %v", err)
		}
		if !holds {
			t.Errorf("human: got not holding the role, want holding it")
		}
		if holds, _ := d.HoldsRole(ctx, ten.Agent.ID, role); holds {
			t.Error("the agent holds the human role")
		}
		roles, err := d.RolesHeld(ctx, ten.Human.ID)
		if err != nil {
			t.Errorf("roles held: %v", err)
		}
		if len(roles) != 1 || roles[0] != role {
			t.Errorf("roles held: got %v, want [%s]", roles, role)
		}
		named, err := d.Roles(ctx, []uuid.UUID{role, uuid.New()})
		if err != nil {
			t.Errorf("roles: %v", err)
		}
		if got := fmt.Sprint(named); got != fmt.Sprintf("map[%s:{all All active}]", role) {
			t.Errorf("roles: got %s, want the role alone", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"roles": func(ctx context.Context) error { _, err := d.Roles(ctx, []uuid.UUID{uuid.New()}); return err },
		"role":  func(ctx context.Context) error { _, _, err := d.RoleByCode(ctx, "all"); return err },
		"holds": func(ctx context.Context) error { _, err := d.HoldsRole(ctx, ten.Human.ID, uuid.New()); return err },
		"held":  func(ctx context.Context) error { _, err := d.RolesHeld(ctx, ten.Human.ID); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, ten, ten.Human, name, call)
		})
	}
}

// TestDirectoryInactiveRole: a role made inactive gives its holders
// nothing here either, as authz grants it nothing: HoldsRole denies it and
// RolesHeld leaves it out until it is active again.
func TestDirectoryInactiveRole(t *testing.T) {
	ten := pgtest.NewTenant(t)
	retired := uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, 'retired', 'Retired', 'active', $3, $3)`,
		ten.ID, retired, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`,
		ten.ID, ten.Human.ID, retired)
	var all uuid.UUID
	pgtest.Scan(t, `SELECT id FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, []any{ten.ID}, &all)
	byID := func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }
	for _, c := range []struct {
		status string
		holds  bool
		held   []uuid.UUID
	}{{"inactive", false, []uuid.UUID{all}}, {"active", true, []uuid.UUID{all, retired}}} {
		t.Run(c.status, func(t *testing.T) {
			pgtest.Exec(t, `UPDATE identity.role SET status = $2, version = version + 1 WHERE id = $1`, retired, c.status)
			err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
				holds, err := identity.Directory{}.HoldsRole(ctx, ten.Human.ID, retired)
				if err != nil {
					return err
				}
				if holds != c.holds {
					t.Errorf("holds the %s role: got %v, want %v", c.status, holds, c.holds)
				}
				held, err := identity.Directory{}.RolesHeld(ctx, ten.Human.ID)
				if err != nil {
					return err
				}
				slices.SortFunc(held, byID)
				slices.SortFunc(c.held, byID)
				if !slices.Equal(held, c.held) {
					t.Errorf("roles held with the role %s: got %v, want %v", c.status, held, c.held)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestActorsActive: a principal is active until it is blocked; an unknown
// one is an error.
func TestActorsActive(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, ten.Agent.ID)
	err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		for name, c := range map[string]struct {
			id   uuid.UUID
			want bool
		}{"active": {ten.Human.ID, true}, "blocked": {ten.Agent.ID, false}} {
			t.Run(name, func(t *testing.T) {
				if got, err := (identity.Actors{}).Active(ctx, tx, c.id); err != nil || got != c.want {
					t.Errorf("got %v %v, want %v", got, err, c.want)
				}
			})
		}
		if _, err := (identity.Actors{}).Active(ctx, tx, uuid.New()); err == nil {
			t.Error("an unknown principal: got no error, want one")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestActorsMayAct: work may act as an active principal; one blocked or
// deprovisioned since is refused with identity.principal_blocked, 403, as
// its requests are; an unknown one is an error, not a refusal.
func TestActorsMayAct(t *testing.T) {
	ten := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, ten.Agent.ID)
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'deprovisioned', version = version + 1 WHERE id = $1`, ten.Service.ID)
	err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		tx, _ := reqctx.TxFrom(ctx)
		if err := (identity.Actors{}).MayAct(ctx, tx, ten.Human.ID); err != nil {
			t.Errorf("active: got %v, want nil", err)
		}
		for name, id := range map[string]uuid.UUID{"blocked": ten.Agent.ID, "deprovisioned": ten.Service.ID} {
			t.Run(name, func(t *testing.T) {
				var why *problem.Problem
				err := (identity.Actors{}).MayAct(ctx, tx, id)
				if !errors.As(err, &why) {
					t.Fatalf("got %v, want a problem", err)
				}
				if got := fmt.Sprint(why.Status, " ", why.Code); got != "403 identity.principal_blocked" {
					t.Errorf("got %s, want 403 identity.principal_blocked", got)
				}
				if !strings.Contains(why.Detail, name) {
					t.Errorf("detail: got %q, want the status %s named", why.Detail, name)
				}
			})
		}
		var why *problem.Problem
		unknown := (identity.Actors{}).MayAct(ctx, tx, uuid.New())
		if unknown == nil {
			t.Error("an unknown principal: got nil, want an error")
		}
		if errors.As(unknown, &why) {
			t.Errorf("an unknown principal: got the problem %s, want an error that is no problem", why.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
