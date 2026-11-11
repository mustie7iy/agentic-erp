package ledger

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestUnassignedBelowARootCodedSo: a standard hierarchy whose root is coded
// UNASSIGNED takes no UNASSIGNED leaf, which would repeat its code; the
// approval is refused before any statement.
func TestUnassignedBelowARootCodedSo(t *testing.T) {
	root := uuid.New()
	tr := tree{Hierarchy: Hierarchy{RootID: root}, nodes: map[uuid.UUID]*HierarchyNode{root: {ID: root, Code: unassignedCode}}}
	_, err := addUnassigned(context.Background(), nil, tr)
	if p := problem.From(err); p.Code != "ledger.hierarchy_change_invalid" || p.Errors[0].Code != reasonUnknown {
		t.Errorf("got %v, want ledger.hierarchy_change_invalid unknown", err)
	}
}
