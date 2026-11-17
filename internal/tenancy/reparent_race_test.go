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

// TestConcurrentMovesKeepTheTreeAcyclic: two moves made at once that
// close a cycle only together (b is under top; a goes under b while top goes
// under a) cannot both commit. The second waits for the first's lock on
// the legal entity's tree and its check then sees the first's move, so it
// is refused as a cycle. (Two units moved under each other are kept apart
// by the parent's foreign key lock already; three are not: the second
// waits for that lock only after its check.)
func TestConcurrentMovesKeepTheTreeAcyclic(t *testing.T) {
	ten := org(t)
	c := newClient(t, ten, ten.Human)
	le := c.legalEntity("DE01")
	unit := func(code string) uuid.UUID {
		m, _ := c.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": %q, "name": "P"}`, le["id"], code), "")
		return uuid.MustParse(m["id"].(string))
	}
	a, b, top := unit("PA"), unit("PB"), unit("PC")
	c.Must(200, "PATCH", "/v1/org-units/"+b.String(), fmt.Sprintf(`{"parent_id": %q}`, top), `"1"`)
	move := func(id, parent uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := tenancy.UpdateOrgUnit(ctx, id, func(int32) error { return nil }, tenancy.OrgUnitChange{Parent: true, ParentID: &parent})
			return err
		}
	}
	first, second := apitest.Race(t, ten, ten.Human, ten.Human, move(a, b), move(top, a))
	if first != nil {
		t.Fatalf("first move: got %v, want it made", first)
	}
	if got := problem.From(second); got.Code != "tenancy.invalid_parent" {
		t.Fatalf("second move: got %v, want tenancy.invalid_parent (a cycle)", second)
	}
	var withParent int // all three with a parent is the cycle
	pgtest.Scan(t, `SELECT count(*) FROM tenancy.org_unit WHERE id IN ($1, $2, $3) AND parent_id IS NOT NULL`, []any{a, b, top}, &withParent)
	if withParent != 2 {
		t.Errorf("got %d of the three units with a parent, want 2 (a under b under top)", withParent)
	}
}
