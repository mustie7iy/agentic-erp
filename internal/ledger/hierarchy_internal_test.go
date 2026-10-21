package ledger

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestUnassignedBelowAFullRoot: a standard hierarchy whose root, still a
// leaf, holds centers takes no UNASSIGNED leaf below it; the request is
// refused before any statement (a tree only migrations or tests leave).
func TestUnassignedBelowAFullRoot(t *testing.T) {
	root := uuid.New()
	tr := tree{Hierarchy: Hierarchy{RootID: root}, nodes: map[uuid.UUID]*HierarchyNode{root: {ID: root, Centers: []uuid.UUID{uuid.New()}}}}
	_, err := addUnassigned(context.Background(), nil, tr)
	if p := problem.From(err); p.Code != "ledger.hierarchy_change_invalid" || p.Errors[0].Code != reasonHasCenters {
		t.Errorf("got %v, want ledger.hierarchy_change_invalid has_centers", err)
	}
}
