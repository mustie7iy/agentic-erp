package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Every change to a hierarchy's tree is a center change a controller
// approves (ADR-0050 §11, T56b): planned against the tree when requested,
// planned again and written when approved. A hierarchy itself is created
// empty, with its root, at once: an empty tree changes no center.

// NewHierarchy is a hierarchy to create.
type NewHierarchy struct {
	Kind       string
	Code       string
	Name       string
	IsStandard bool
}

// CreateHierarchy creates a hierarchy and its root node, which takes the
// hierarchy's code and name.
func CreateHierarchy(ctx context.Context, in NewHierarchy) (Hierarchy, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Hierarchy{}, err
	}
	if _, ok := centerKinds[in.Kind]; !ok {
		return Hierarchy{}, problem.Unprocessable("request.validation_failed", "A hierarchy holds cost or profit centers.",
			problem.FieldError{Field: "kind", Code: "invalid", Message: "cost_center or profit_center"})
	}
	id, root := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.hierarchy (tenant_id, id, kind, code, name, is_standard, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, p.TenantID, id, in.Kind, in.Code, in.Name, in.IsStandard, p.ID); err != nil {
		return Hierarchy{}, fmt.Errorf("ledger: insert hierarchy %s: %w", in.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, code, name, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, p.TenantID, root, id, in.Code, in.Name, p.ID); err != nil {
		return Hierarchy{}, fmt.Errorf("ledger: insert root of hierarchy %s: %w", in.Code, err)
	}
	h, err := getHierarchy(ctx, tx, byID, false, id)
	if err != nil {
		return Hierarchy{}, err
	}
	return h, recordHierarchy(ctx, h, "create", nil, nil, nil)
}

// RequestHierarchyChange opens a change of a hierarchy's tree: a node
// added, renamed, moved or removed, or (center set) a center placed or
// taken out. The center is locked before the hierarchy, as when the
// change is decided.
func (e *Endpoints) RequestHierarchyChange(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, kind string, center *uuid.UUID,
	p hierarchyPayload, reason string) (CenterChange, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CenterChange{}, err
	}
	h, err := getHierarchy(ctx, tx, byID, false, id)
	if err != nil {
		return CenterChange{}, err
	}
	ref := centerRef{kind: centerKinds[h.Kind], hierarchy: &h.ID, title: "Hierarchy " + h.Code + " (" + h.Kind + ")"}
	if center != nil {
		ref.id = *center
		if err := lockPlaced(ctx, tx, ref); err != nil {
			return CenterChange{}, err
		}
	}
	t, err := loadTree(ctx, tx, byID, true, id)
	if err != nil {
		return CenterChange{}, err
	}
	if err := ifMatch(t.Version); err != nil {
		return CenterChange{}, err
	}
	if kind == KindNodeCreate {
		node := uuid.Must(uuid.NewV7())
		p.NodeID = &node
	}
	if err := t.plan(kind, ref.id, p); err != nil {
		return CenterChange{}, err
	}
	return requestChange(ctx, tx, e.Approvals, ref, kind, p, reason)
}

// lockPlaced locks the center a place names and checks it is active: a
// center outside the caller's companies, or of another kind, is not a
// center the request can name.
func lockPlaced(ctx context.Context, tx pgx.Tx, ref centerRef) error {
	var status string
	var err error
	if ref.kind == costCenters {
		var access authz.Access
		if access, err = authz.Companies(ctx); err != nil {
			return err
		}
		var c CostCenter
		c, err = getCostCenter(ctx, tx, access, ref.id, true)
		status = c.Status
	} else {
		var c ProfitCenter
		c, err = getProfitCenter(ctx, tx, ref.id, forUpdate)
		status = c.Status
	}
	if err = problem.Reference(err, "center_id", "center", "a center of the hierarchy's kind"); err != nil {
		return err
	}
	return changeable(centerHead{Status: status}, func(int32) error { return nil })
}

// placeOf is the center a place change names, uuid.Nil for a node change.
func placeOf(ch CenterChange) uuid.UUID {
	if _, c := ch.center(); c != nil {
		return *c
	}
	return uuid.Nil
}

// applyHierarchyChange plans an approved change against the tree as it now
// is and writes it.
func applyHierarchyChange(ctx context.Context, tx pgx.Tx, ch CenterChange) error {
	t, err := loadTree(ctx, tx, byID, true, *ch.HierarchyID)
	if err != nil {
		return err
	}
	var p hierarchyPayload
	decode(ch.Payload, &p)
	center := placeOf(ch)
	if err := t.plan(ch.Kind, center, p); err != nil {
		return err
	}
	if err := writeTree(ctx, tx, t, ch, center, p); err != nil {
		return err
	}
	// A place's center loses its pending change: its representation, and
	// so its ETag, moves on as when the change is closed.
	k, placed := ch.center()
	if placed != nil {
		if err := k.touch(ctx, tx, *placed, ""); err != nil {
			return err
		}
	}
	return touchTree(ctx, tx, t.ID, ch.Kind, &ch.ID, p.NodeID, placed)
}

// writeTree writes a planned change. Nothing is deleted: a removed node
// or place is marked with the change that removed it.
func writeTree(ctx context.Context, tx pgx.Tx, t tree, ch CenterChange, center uuid.UUID, p hierarchyPayload) error {
	var q string
	var args []any
	switch ch.Kind {
	case KindNodeCreate:
		q = `INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, parent_id, code, name, position, change_id, created_by, updated_by)
			SELECT tenant_id, $2, id, $3, $4, $5, $6, $7, $1, $1 FROM ledger.hierarchy WHERE id = $8`
		args = []any{*p.NodeID, p.ParentID, p.Code, p.Name, nodePosition(t, *p.ParentID, p.Position), ch.ID, t.ID}
	case KindNodeRename:
		q, args = `UPDATE ledger.hierarchy_node SET name = $3, version = version + 1, updated_by = $1 WHERE id = $2`, []any{*p.NodeID, p.Name}
	case KindNodeMove:
		q = `UPDATE ledger.hierarchy_node SET parent_id = $3, position = $4, version = version + 1, updated_by = $1 WHERE id = $2`
		args = []any{*p.NodeID, p.ParentID, nodePosition(t, *p.ParentID, p.Position)}
	case KindNodeRemove:
		q = `UPDATE ledger.hierarchy_node SET removed_by_change_id = $3, version = version + 1, updated_by = $1 WHERE id = $2`
		args = []any{*p.NodeID, ch.ID}
	default:
		q, args = placeSQL(t, ch.ID, center, p.NodeID)
	}
	_, err := tx.Exec(ctx, q, append([]any{actor(ctx)}, args...)...)
	return errs.Wrapf(err, "ledger: write %s of hierarchy %s", ch.Kind, t.ID)
}

// nodePosition is where a node goes among its new siblings: as asked, or
// after them.
func nodePosition(t tree, parent uuid.UUID, asked *int32) int32 {
	if asked != nil {
		return *asked
	}
	return t.nextPosition(parent)
}

// placeSQL places a center at a node, moves it there, or takes it out of
// the hierarchy ($1 is the actor).
func placeSQL(t tree, change, center uuid.UUID, node *uuid.UUID) (string, []any) {
	_, placed := t.placed[center]
	switch {
	case node == nil:
		return `UPDATE ledger.hierarchy_member SET removed_by_change_id = $2, version = version + 1, updated_by = $1
			WHERE hierarchy_id = $3 AND coalesce(cost_center_id, profit_center_id) = $4 AND removed_by_change_id IS NULL`, []any{change, t.ID, center}
	case placed:
		return `UPDATE ledger.hierarchy_member SET node_id = $2, version = version + 1, updated_by = $1
			WHERE hierarchy_id = $3 AND coalesce(cost_center_id, profit_center_id) = $4 AND removed_by_change_id IS NULL`, []any{*node, t.ID, center}
	}
	cc, pc := &center, (*uuid.UUID)(nil)
	if t.Kind == HierarchyProfitCenters {
		cc, pc = nil, &center
	}
	return `INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, cost_center_id, profit_center_id, change_id, created_by, updated_by)
		SELECT tenant_id, gen_random_uuid(), id, kind, $2, $3, $4, $5, $1, $1 FROM ledger.hierarchy WHERE id = $6`, []any{*node, cc, pc, change, t.ID}
}

// touchTree moves the hierarchy's version, its ETag, on and records the
// change.
func touchTree(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string, change, node, center *uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE ledger.hierarchy SET version = version + 1, updated_by = $2 WHERE id = $1`, id, actor(ctx)); err != nil {
		return fmt.Errorf("ledger: update hierarchy %s: %w", id, err)
	}
	h, err := getHierarchy(ctx, tx, byID, false, id)
	if err != nil {
		return err
	}
	return recordHierarchy(ctx, h, action, change, node, center)
}

// closedHierarchyChange ends a change of a tree without applying it; a
// place's center no longer has a pending change.
func closedHierarchyChange(ctx context.Context, tx pgx.Tx, ch CenterChange) error {
	k, center := ch.center()
	if center == nil {
		return nil
	}
	return k.touch(ctx, tx, *center, "")
}

// recordHierarchy audits a change of the hierarchy and publishes
// HierarchyChanged: what changed, not the tree, which GET returns.
func recordHierarchy(ctx context.Context, h Hierarchy, action string, change, node, center *uuid.UUID) error {
	payload := map[string]any{"hierarchy_id": h.ID, "kind": h.Kind, "code": h.Code, "is_standard": h.IsStandard, "version": h.Version,
		"change": action, "center_change_id": change, "node_id": node, "center_id": center}
	return audit.Record(ctx, "ledger.hierarchy."+action, "ledger.hierarchy", h.ID,
		map[string]any{"center_change_id": change, "node_id": node, "center_id": center},
		outbox.Event{Context: "ledger", Name: "hierarchy_changed", AggregateType: "ledger.hierarchy", AggregateID: h.ID, Payload: payload})
}

// standardTree reads the tenant's standard hierarchy of a kind; field
// names the node a request gave, which cannot be in a hierarchy that does
// not exist.
func standardTree(ctx context.Context, tx pgx.Tx, kind string, lock bool, field string) (tree, error) {
	t, err := loadTree(ctx, tx, `h.kind = $1 AND h.is_standard`, lock, kind)
	if err != nil && problem.From(err).Code == "request.not_found" {
		return tree{}, invalidChange(field, reasonUnknown, "the tenant has no standard hierarchy of this kind")
	}
	return t, err
}

// checkStandardNode checks the node a new center is to be placed at: a
// leaf of the standard hierarchy of its kind.
func checkStandardNode(ctx context.Context, tx pgx.Tx, kind string, center, node uuid.UUID) error {
	t, err := standardTree(ctx, tx, kind, false, "standard_node_id")
	if err != nil {
		return err
	}
	return t.planPlace("standard_node_id", center, &node)
}

// unassignedCode is the standard hierarchy's leaf a center created
// without a node is placed at, for a controller to place later (product
// owner, 2026-10-21), as migration 000139 placed the centers before T56b.
const unassignedCode = "UNASSIGNED"

// unassignedLeaf is the standard hierarchy's UNASSIGNED leaf, which takes a
// center whose approved create named no leaf: added below the root when
// the tree has none, and created with the standard hierarchy when the
// tenant has none, so that a center never waits on a tree being built
// first. It runs when the create is approved, by the approver, so that
// proposing a center (x-autonomy: propose) changes no tree. Approvals of
// two first centers are serialized per tenant and kind: the second finds
// what the first created.
func unassignedLeaf(ctx context.Context, tx pgx.Tx, kind string) (uuid.UUID, error) {
	t, err := standardOrNew(ctx, tx, kind)
	if err != nil {
		if problem.From(err).Code == "ledger.hierarchy_exists" {
			return uuid.Nil, invalidChange("standard_node_id", reasonUnknown,
				"the tenant has no standard hierarchy of this kind and another hierarchy has the code STANDARD; create a standard hierarchy first, or reject the creation and request it again with standard_node_id")
		}
		return uuid.Nil, err
	}
	for id, n := range t.nodes {
		if n.Code != unassignedCode || n.ParentID == nil {
			continue
		}
		if len(t.children[id]) > 0 {
			return uuid.Nil, invalidChange("standard_node_id", reasonHasChildren, "the standard hierarchy's UNASSIGNED node has child nodes; move them below another node first, or reject the creation and request it again with standard_node_id")
		}
		return id, nil
	}
	return addUnassigned(ctx, tx, t)
}

// standardOrNew is the tenant's standard hierarchy of a kind, locked,
// created coded STANDARD, with its root, when the tenant has none
// (ledger.hierarchy_exists when another hierarchy of the kind has that
// code). Its callers are serialized per tenant and kind by an advisory
// lock, so the second finds what the first created.
func standardOrNew(ctx context.Context, tx pgx.Tx, kind string) (tree, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ledger.standard_hierarchy:' || platform.require_tenant() || ':' || $1, 0))`,
		kind); err != nil {
		return tree{}, fmt.Errorf("ledger: lock the standard hierarchy of %s: %w", kind, err)
	}
	t, err := loadTree(ctx, tx, `h.kind = $1 AND h.is_standard`, true, kind)
	if err != nil && problem.From(err).Code == "request.not_found" {
		if _, err := CreateHierarchy(ctx, NewHierarchy{Kind: kind, Code: "STANDARD", Name: "Standard hierarchy", IsStandard: true}); err != nil {
			return tree{}, err
		}
		t, err = loadTree(ctx, tx, `h.kind = $1 AND h.is_standard`, true, kind)
	}
	return t, err
}

// addUnassigned adds the UNASSIGNED leaf below the standard hierarchy's
// root, as migration 000139 does: no change approves it, as none approves
// the root, since a leaf without centers changes no center.
func addUnassigned(ctx context.Context, tx pgx.Tx, t tree) (uuid.UUID, error) {
	root := t.nodes[t.RootID]
	if len(root.Centers) > 0 {
		return uuid.Nil, invalidChange("standard_node_id", reasonHasCenters,
			"centers sit at the standard hierarchy's root, which takes no child node; reject the creation and request it again with standard_node_id")
	}
	if root.Code == unassignedCode { // node codes are unique in a hierarchy
		return uuid.Nil, invalidChange("standard_node_id", reasonUnknown,
			"the standard hierarchy's root is coded UNASSIGNED, so it takes no UNASSIGNED leaf; reject the creation and request it again with standard_node_id")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, parent_id, code, name, position, created_by, updated_by)
		SELECT tenant_id, $2, id, $3, $4, 'Unassigned', $5, $1, $1 FROM ledger.hierarchy WHERE id = $6`,
		actor(ctx), id, t.RootID, unassignedCode, t.nextPosition(t.RootID), t.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: add the UNASSIGNED leaf of hierarchy %s: %w", t.ID, err)
	}
	return id, touchTree(ctx, tx, t.ID, KindNodeCreate, nil, &id, nil)
}

// placeCreated places a center whose create was approved at the node its
// request named, checked again against the standard tree as it now is,
// or, when it named none, at the UNASSIGNED leaf (unassignedLeaf).
func placeCreated(ctx context.Context, tx pgx.Tx, kind string, center, change uuid.UUID, node *uuid.UUID) error {
	if node == nil {
		leaf, err := unassignedLeaf(ctx, tx, kind)
		if err != nil {
			return err
		}
		node = &leaf
	}
	t, err := standardTree(ctx, tx, kind, true, "standard_node_id")
	if err != nil {
		return err
	}
	if err := t.planPlace("standard_node_id", center, node); err != nil {
		return err
	}
	q, args := placeSQL(t, change, center, node)
	if _, err := tx.Exec(ctx, q, append([]any{actor(ctx)}, args...)...); err != nil {
		return fmt.Errorf("ledger: place %s %s in hierarchy %s: %w", kind, center, t.ID, err)
	}
	return touchTree(ctx, tx, t.ID, KindPlace, &change, node, &center)
}

// activate makes a created center active and places it at the leaf of the
// standard hierarchy its request named; the database checks at commit
// that every active center is placed.
func activate(ctx context.Context, tx pgx.Tx, k centerKind, center, change uuid.UUID, node *uuid.UUID) error {
	if err := k.touch(ctx, tx, center, "status = 'active', "); err != nil {
		return err
	}
	return placeCreated(ctx, tx, k.table, center, change, node)
}
