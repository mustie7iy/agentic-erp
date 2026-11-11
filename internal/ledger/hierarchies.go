package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Hierarchies of cost and profit centers (ADR-0050 §3, T56b): trees of
// nodes with centers of the hierarchy's kind at their nodes, an inner node
// included, as SAP's groups hold centers beside subgroups (the SAP
// specialist, po:2026-11-09, A10(d)). A tenant has at most one standard
// hierarchy per kind, and every active center sits in exactly one node of
// it; alternative hierarchies hold a center at most once. Hierarchies are
// not time-dependent (decision P7). The database keeps these rules
// (migrations 000139, 000247); planning a change checks them
// first, so that a request is refused with a reason rather than by a
// trigger, and checks them again when the change is applied.

// Hierarchy kinds: the kind of center a hierarchy holds, named as the
// center's table.
const (
	HierarchyCostCenters   = "cost_center"
	HierarchyProfitCenters = "profit_center"
)

// Hierarchy change kinds (center_change.kind).
const (
	KindNodeCreate = "node_create"
	KindNodeRename = "node_rename"
	KindNodeMove   = "node_move"
	KindNodeRemove = "node_remove"
	KindPlace      = "place"
)

// maxLevels is the deepest a tree goes, its root at level 1 (ADR-0050 §3).
const maxLevels = 12

func init() {
	problem.RegisterConstraint("hierarchy_one_standard", func() *problem.Problem {
		return problem.Conflict("ledger.standard_hierarchy_exists", "The tenant already has a standard hierarchy of this kind.")
	})
	problem.RegisterConstraint("hierarchy_code_once", func() *problem.Problem {
		return problem.Conflict("ledger.hierarchy_exists", "The tenant already has a hierarchy of this kind with this code.")
	})
}

// Hierarchy is a row of ledger.hierarchy with its root node.
type Hierarchy struct {
	ID         uuid.UUID
	Kind       string
	Code       string
	Name       string
	IsStandard bool
	RootID     uuid.UUID
	Version    int32
	CreatedAt  time.Time
	CreatedBy  uuid.UUID
	UpdatedAt  time.Time
	UpdatedBy  uuid.UUID
}

// HierarchyNode is a node of a tree with the centers placed at it.
type HierarchyNode struct {
	ID       uuid.UUID
	ParentID *uuid.UUID
	Code     string
	Name     string
	Position int32
	Centers  []uuid.UUID
}

// hierarchySelect reads hierarchies; it has one table in FROM, so that
// keyset paging's id is the hierarchy's.
const hierarchySelect = `SELECT h.id, h.kind, h.code, h.name, h.is_standard,
		(SELECT r.id FROM ledger.hierarchy_node r WHERE r.tenant_id = h.tenant_id AND r.hierarchy_id = h.id AND r.parent_id IS NULL) AS root_node_id,
		h.version, h.created_at, h.created_by, h.updated_at, h.updated_by FROM ledger.hierarchy h`

func scanHierarchy(row pgx.Row) (Hierarchy, error) {
	var h Hierarchy
	err := row.Scan(&h.ID, &h.Kind, &h.Code, &h.Name, &h.IsStandard, &h.RootID, &h.Version, &h.CreatedAt, &h.CreatedBy, &h.UpdatedAt, &h.UpdatedBy)
	return h, errs.Wrapf(err, "scan hierarchy")
}

// getHierarchy reads a hierarchy, locked for update when lock is set: a
// change to its tree holds the lock from planning to writing, so changes
// to one tree are planned one after the other.
func getHierarchy(ctx context.Context, tx pgx.Tx, where string, lock bool, args ...any) (Hierarchy, error) {
	q := hierarchySelect + ` WHERE ` + where
	if lock {
		q += ` FOR UPDATE OF h`
	}
	h, err := scanHierarchy(tx.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Hierarchy{}, problem.NotFoundOf("hierarchy")
	}
	if err != nil {
		return Hierarchy{}, fmt.Errorf("ledger: read hierarchy %v: %w", args, err)
	}
	return h, nil
}

const byID = `h.id = $1`

// tree is a hierarchy's nodes and places as one transaction reads them.
type tree struct {
	Hierarchy
	nodes    map[uuid.UUID]*HierarchyNode
	children map[uuid.UUID][]uuid.UUID // in order: position, then code
	placed   map[uuid.UUID]uuid.UUID   // center: its node
}

// loadTree reads a hierarchy and its tree, locking the hierarchy when lock
// is set.
func loadTree(ctx context.Context, tx pgx.Tx, where string, lock bool, args ...any) (tree, error) {
	h, err := getHierarchy(ctx, tx, where, lock, args...)
	if err != nil {
		return tree{}, err
	}
	t := tree{Hierarchy: h, nodes: map[uuid.UUID]*HierarchyNode{}, children: map[uuid.UUID][]uuid.UUID{}, placed: map[uuid.UUID]uuid.UUID{}}
	rows, _ := tx.Query(ctx, `SELECT id, parent_id, code, name, position FROM ledger.hierarchy_node
		WHERE hierarchy_id = $1 AND removed_by_change_id IS NULL ORDER BY position, code`, h.ID)
	var n HierarchyNode
	if _, err := pgx.ForEachRow(rows, []any{&n.ID, &n.ParentID, &n.Code, &n.Name, &n.Position}, func() error {
		node := n
		t.nodes[n.ID] = &node
		if n.ParentID != nil {
			t.children[*n.ParentID] = append(t.children[*n.ParentID], n.ID)
		}
		return nil
	}); err != nil {
		return tree{}, fmt.Errorf("ledger: read nodes of hierarchy %s: %w", h.ID, err)
	}
	rows, _ = tx.Query(ctx, `SELECT node_id, coalesce(cost_center_id, profit_center_id) FROM ledger.hierarchy_member
		WHERE hierarchy_id = $1 AND removed_by_change_id IS NULL ORDER BY 2`, h.ID)
	var node, center uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&node, &center}, func() error {
		t.placed[center] = node
		t.nodes[node].Centers = append(t.nodes[node].Centers, center)
		return nil
	}); err != nil {
		return tree{}, fmt.Errorf("ledger: read centers of hierarchy %s: %w", h.ID, err)
	}
	return t, nil
}

// level is a node's depth, the root's 1.
func (t tree) level(id uuid.UUID) int {
	n := 1
	for p := t.nodes[id].ParentID; p != nil; p = t.nodes[*p].ParentID {
		n++
	}
	return n
}

// height is the number of levels of a node's subtree, the node's included.
func (t tree) height(id uuid.UUID) int {
	h := 0
	for _, c := range t.children[id] {
		h = max(h, t.height(c))
	}
	return h + 1
}

// within reports whether node is top or below it.
func (t tree) within(top, node uuid.UUID) bool {
	for at := &node; at != nil; at = t.nodes[*at].ParentID {
		if *at == top {
			return true
		}
	}
	return false
}

// nodeAt is a node in tree order with its level.
type nodeAt struct {
	*HierarchyNode
	Level int
}

// subtree lists the node and its descendants in tree order: each after
// its parent, siblings by position and code.
func (t tree) subtree(top uuid.UUID) []nodeAt {
	var out []nodeAt
	var walk func(id uuid.UUID, level int)
	walk = func(id uuid.UUID, level int) {
		out = append(out, nodeAt{t.nodes[id], level})
		for _, c := range t.children[id] {
			walk(c, level+1)
		}
	}
	walk(top, t.level(top))
	return out
}

// nextPosition is the position after a node's children; a first child
// takes 1, so that position 0 puts a node before every appended one.
func (t tree) nextPosition(parent uuid.UUID) int32 {
	next := int32(1)
	for _, c := range t.children[parent] {
		next = max(next, t.nodes[c].Position+1)
	}
	return next
}

// hierarchyPayload is what a change to a tree asks for. NodeID is the
// node a node change changes (for node_create the new node's id, chosen
// when it is requested) or the node a center is placed at (nil: out of
// the hierarchy); ParentID a new node's or a moved node's parent.
type hierarchyPayload struct {
	NodeID   *uuid.UUID `json:"node_id,omitempty"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	Code     string     `json:"code,omitempty"`
	Name     string     `json:"name,omitempty"`
	Position *int32     `json:"position,omitempty"`
}

// Reasons a change to a tree is refused (the field error's code).
const (
	reasonUnknown     = "unknown"      // not a node of the hierarchy
	reasonRoot        = "root"         // the root is not moved or removed
	reasonHasCenters  = "has_centers"  // a node holding centers is not removed
	reasonHasChildren = "has_children" // a node with children is not removed
	reasonCycle       = "cycle"        // a node moved below itself
	reasonTooDeep     = "too_deep"     // more than maxLevels
	reasonStandard    = "standard"     // a center does not leave the standard hierarchy
)

func invalidChange(field, reason, message string) error {
	return problem.Unprocessable("ledger.hierarchy_change_invalid", "The hierarchy cannot be changed so: "+message+".",
		problem.FieldError{Field: field, Code: reason, Message: message})
}

// node is a node of the tree, or the refusal naming the field.
func (t tree) node(field string, id *uuid.UUID) (*HierarchyNode, error) {
	if id != nil {
		if n, ok := t.nodes[*id]; ok {
			return n, nil
		}
	}
	return nil, invalidChange(field, reasonUnknown, "not a node of the hierarchy")
}

// plan checks a change against the tree as it is; center is a place's.
func (t tree) plan(kind string, center uuid.UUID, p hierarchyPayload) error {
	switch kind {
	case KindNodeCreate:
		return t.planAdd(p)
	case KindNodeRename:
		n, err := t.node("node_id", p.NodeID)
		if err == nil && n.Name == p.Name {
			err = nothingToChange()
		}
		return err
	case KindNodeMove:
		return t.planMove(p)
	case KindNodeRemove:
		return t.planRemove(p)
	}
	return t.planPlace("node_id", center, p.NodeID)
}

// parent checks a node to be given a child: the child's subtree of height
// levels stays within maxLevels. A node holding centers takes children.
func (t tree) parent(field string, id *uuid.UUID, height int) (*HierarchyNode, error) {
	n, err := t.node(field, id)
	switch {
	case err != nil:
		return nil, err
	case t.level(n.ID)+height > maxLevels:
		return nil, invalidChange(field, reasonTooDeep, fmt.Sprintf("a hierarchy has at most %d levels", maxLevels))
	}
	return n, nil
}

func (t tree) planAdd(p hierarchyPayload) error {
	if _, err := t.parent("node_id", p.ParentID, 1); err != nil {
		return err
	}
	for _, n := range t.nodes {
		if n.Code == p.Code {
			return problem.Conflict("ledger.hierarchy_node_exists", "The hierarchy already has a node with this code.")
		}
	}
	return nil
}

func (t tree) planMove(p hierarchyPayload) error {
	n, err := t.node("node_id", p.NodeID)
	if err != nil {
		return err
	}
	if n.ParentID == nil {
		return invalidChange("node_id", reasonRoot, "the root is not moved")
	}
	if _, err := t.node("parent_id", p.ParentID); err != nil {
		return err
	}
	if t.within(n.ID, *p.ParentID) {
		return invalidChange("parent_id", reasonCycle, "the new parent is the node or below it")
	}
	parent, err := t.parent("parent_id", p.ParentID, t.height(n.ID))
	if err != nil {
		return err
	}
	if parent.ID == *n.ParentID && (p.Position == nil || *p.Position == n.Position) {
		return nothingToChange()
	}
	return nil
}

func (t tree) planRemove(p hierarchyPayload) error {
	n, err := t.node("node_id", p.NodeID)
	switch {
	case err != nil:
		return err
	case n.ParentID == nil:
		return invalidChange("node_id", reasonRoot, "the root is not removed")
	case len(t.children[n.ID]) > 0:
		return invalidChange("node_id", reasonHasChildren, "the node has child nodes")
	case len(n.Centers) > 0:
		return invalidChange("node_id", reasonHasCenters, "the node holds centers")
	}
	return nil
}

// planPlace checks placing a center at a node, or (node nil) taking it
// out of the hierarchy.
func (t tree) planPlace(field string, center uuid.UUID, node *uuid.UUID) error {
	at, placed := t.placed[center]
	if node == nil {
		if t.IsStandard {
			return invalidChange(field, reasonStandard, "every active center sits in a node of the standard hierarchy, which must be named")
		}
		if !placed {
			return nothingToChange()
		}
		return nil
	}
	n, err := t.node(field, node)
	switch {
	case err != nil:
		return err
	case placed && at == n.ID:
		return nothingToChange()
	}
	return nil
}
