package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"role":  func(ctx context.Context) error { _, _, err := d.RoleByCode(ctx, "all"); return err },
		"holds": func(ctx context.Context) error { _, err := d.HoldsRole(ctx, ten.Human.ID, uuid.New()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, ten, ten.Human, name, call)
		})
	}
}
