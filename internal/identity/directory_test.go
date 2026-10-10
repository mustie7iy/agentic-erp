package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
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
		if _, ok, err := d.RoleByCode(ctx, "none"); ok || err != nil {
			t.Errorf("unknown role: %v %v", ok, err)
		}
		if holds, err := d.HoldsRole(ctx, ten.Human.ID, role); !holds || err != nil {
			t.Errorf("human: %v %v", holds, err)
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
		if call(context.Background()) == nil {
			t.Errorf("%s outside a unit of work", name)
		}
		apitest.EveryFailure(t, ten, ten.Human, name, call)
	}
}
