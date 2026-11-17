package tenancy_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestConcurrentMoves: two moves made at once in one legal entity's tree
// run one after the other under the tree's lock. Two that close a cycle
// only together cannot both commit: the second's check sees the first's
// move and refuses it. Two that do not conflict both commit, without a
// deadlock between the unit's row lock and its parent's foreign key lock.
func TestConcurrentMoves(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	le := c.legalEntity("DE01")
	unit := func(code string) uuid.UUID {
		m, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": %q, "name": "P"}`, le["id"], code), "")
		return uuid.MustParse(m["id"].(string))
	}
	move := func(id, parent uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := tenancy.UpdateOrgUnit(ctx, id, func(int32) error { return nil }, tenancy.OrgUnitChange{Parent: true, ParentID: &parent})
			return err
		}
	}
	parentOf := func(id uuid.UUID) *uuid.UUID {
		var p *uuid.UUID
		pgtest.Scan(t, `SELECT parent_id FROM tenancy.org_unit WHERE id = $1`, []any{id}, &p)
		return p
	}
	t.Run("two units under each other", func(t *testing.T) {
		a, b := unit("A1"), unit("B1")
		first, second := apitest.Race(t, ten, ten.Human, ten.Human, move(a, b), move(b, a))
		if first != nil {
			t.Fatalf("first move: got %v, want it made", first)
		}
		if got := problem.From(second).Code; got != "tenancy.invalid_parent" {
			t.Fatalf("second move: got %v, want tenancy.invalid_parent (a cycle)", second)
		}
		if p := parentOf(b); p != nil {
			t.Errorf("b's parent: got %v, want none", *p)
		}
	})
	t.Run("three units closing a cycle", func(t *testing.T) {
		a, b, top := unit("A2"), unit("B2"), unit("T2")
		c.Must(200, "PATCH", "/v1/org-units/"+b.String(), fmt.Sprintf(`{"parent_id": %q}`, top), `"1"`)
		first, second := apitest.Race(t, ten, ten.Human, ten.Human, move(a, b), move(top, a))
		if first != nil {
			t.Fatalf("first move: got %v, want it made", first)
		}
		if got := problem.From(second).Code; got != "tenancy.invalid_parent" {
			t.Fatalf("second move: got %v, want tenancy.invalid_parent (a cycle)", second)
		}
		if p := parentOf(top); p != nil {
			t.Errorf("top's parent: got %v, want none", *p)
		}
	})
	t.Run("a move under a unit being moved", func(t *testing.T) {
		a, p, q := unit("A3"), unit("P3"), unit("Q3")
		first, second := apitest.Race(t, ten, ten.Human, ten.Human, move(p, q), move(a, p))
		if first != nil || second != nil {
			t.Fatalf("got %v and %v, want both moves made", first, second)
		}
		if got := parentOf(a); got == nil || *got != p {
			t.Errorf("a's parent: got %v, want %v", got, p)
		}
	})
}
