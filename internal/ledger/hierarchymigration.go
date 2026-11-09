package ledger

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// SAP's center groups (sets 0101 and 0106) are loaded by an SAP migration
// run through these operations (ADR-0050 §10, T56h2): each node it adds
// and each center it places is a hierarchy change recorded as applied at
// once, naming the run in its reason, with no approval request of its own,
// as the centers the run creates (CenterMigration). They are planned as a
// requested change is when it is approved (tree.plan: at most 12 levels,
// a code once in the tree, a node of it for a center), and audited and
// published as the API's.

// MigratedNode is a node a run adds below Parent, after its siblings; the
// run chooses its id, as a request does (RequestHierarchyChange), so that
// a node may be the parent of one after it.
type MigratedNode struct {
	ID, Parent uuid.UUID
	Code, Name string
}

// MigratedPlace is a center placed at a node.
type MigratedPlace struct {
	Center, Node uuid.UUID
}

// StandardHierarchy is the tenant's standard hierarchy of a kind
// (HierarchyCostCenters, HierarchyProfitCenters) and its root, created
// coded STANDARD when the tenant has none, as a first center's approval
// creates it (unassignedLeaf). A run adds SAP's standard hierarchy below
// that root, so the tree a tenant has, its UNASSIGNED leaf with the
// centers an earlier run placed there included, is kept.
func (CenterMigration) StandardHierarchy(ctx context.Context, kind string) (hierarchy, root uuid.UUID, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	t, err := standardOrNew(ctx, tx, kind)
	return t.ID, t.RootID, err
}

// CreateHierarchy creates an alternative hierarchy and its root, as the
// API does (CreateHierarchy).
func (CenterMigration) CreateHierarchy(ctx context.Context, in NewHierarchy) (hierarchy, root uuid.UUID, err error) {
	h, err := CreateHierarchy(ctx, in)
	return h.ID, h.RootID, err
}

// AddNodes adds the nodes to the hierarchy in their order, each below a
// node of the tree or one added before it.
func (CenterMigration) AddNodes(ctx context.Context, hierarchy uuid.UUID, nodes []MigratedNode, reason string) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	t, err := loadTree(ctx, tx, byID, true, hierarchy)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		id, parent := n.ID, n.Parent
		p := hierarchyPayload{NodeID: &id, ParentID: &parent, Code: n.Code, Name: n.Name}
		if err := t.plan(KindNodeCreate, uuid.Nil, p); err != nil {
			return err
		}
		change, err := appliedChange(ctx, tx, centerRef{kind: centerKinds[t.Kind], hierarchy: &t.ID}, KindNodeCreate, p, reason)
		if err != nil {
			return err
		}
		position := t.nextPosition(parent)
		if err := writeTree(ctx, tx, t, CenterChange{ID: change, Kind: KindNodeCreate}, uuid.Nil, p); err != nil {
			return err
		}
		t.nodes[id] = &HierarchyNode{ID: id, ParentID: &parent, Code: n.Code, Name: n.Name, Position: position}
		t.children[parent] = append(t.children[parent], id)
		if err := touchTree(ctx, tx, t.ID, KindNodeCreate, &change, &id, nil); err != nil {
			return err
		}
	}
	return nil
}

// PlaceCenters places centers the run created at nodes of an alternative
// hierarchy. A center a run created is not yet seen by any other
// transaction, so it is not locked before the hierarchy, as the API locks
// a center it places (lockPlaced); a run places no center an earlier run
// loaded (integration keeps what is loaded).
func (CenterMigration) PlaceCenters(ctx context.Context, hierarchy uuid.UUID, places []MigratedPlace, reason string) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	t, err := loadTree(ctx, tx, byID, true, hierarchy)
	if err != nil {
		return err
	}
	k := centerKinds[t.Kind]
	for _, pl := range places {
		center, node := pl.Center, pl.Node
		p := hierarchyPayload{NodeID: &node}
		if err := t.plan(KindPlace, center, p); err != nil {
			return err
		}
		change, err := appliedChange(ctx, tx, centerRef{kind: k, id: center, hierarchy: &t.ID}, KindPlace, p, reason)
		if err != nil {
			return err
		}
		if err := writeTree(ctx, tx, t, CenterChange{ID: change, Kind: KindPlace}, center, p); err != nil {
			return err
		}
		t.placed[center] = node
		t.nodes[node].Centers = append(t.nodes[node].Centers, center)
		if err := k.touch(ctx, tx, center, ""); err != nil {
			return err
		}
		if err := touchTree(ctx, tx, t.ID, KindPlace, &change, &node, &center); err != nil {
			return err
		}
	}
	return nil
}
