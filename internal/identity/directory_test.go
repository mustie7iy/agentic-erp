package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
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
