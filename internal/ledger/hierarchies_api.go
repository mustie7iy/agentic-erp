package ledger

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func hierarchyToAPI(h Hierarchy) gen.Hierarchy {
	return gen.Hierarchy{ID: h.ID, Version: h.Version, CreatedAt: h.CreatedAt, CreatedBy: h.CreatedBy, UpdatedAt: h.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(h.UpdatedBy), Kind: h.Kind, Code: h.Code, Name: h.Name, IsStandard: h.IsStandard, RootNodeID: h.RootID}
}

// visibleTree reads a tree for the API: the cost centers of companies the
// caller may not act in are left out of its nodes, as from every other
// read of cost centers; profit centers are tenant-wide.
func visibleTree(ctx context.Context, tx pgx.Tx, id uuid.UUID) (tree, error) {
	t, err := loadTree(ctx, tx, byID, false, id)
	if err != nil || t.Kind != HierarchyCostCenters {
		return t, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return tree{}, err
	}
	rows, _ := tx.Query(ctx, `SELECT id FROM ledger.cost_center WHERE id = ANY($1) AND ($2 OR legal_entity_id = ANY($3))`,
		slices.Collect(maps.Keys(t.placed)), access.All, access.IDs)
	visible, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return tree{}, fmt.Errorf("ledger: cost centers of hierarchy %s: %w", id, err)
	}
	for _, n := range t.nodes {
		n.Centers = slices.DeleteFunc(n.Centers, func(c uuid.UUID) bool { return !slices.Contains(visible, c) })
	}
	return t, nil
}

// treeToAPI is the hierarchy with the subtree below top, top first.
func treeToAPI(t tree, top uuid.UUID) *gen.HierarchyTreeHeaders {
	h := hierarchyToAPI(t.Hierarchy)
	out := gen.HierarchyTree{ID: h.ID, Version: h.Version, CreatedAt: h.CreatedAt, CreatedBy: h.CreatedBy, UpdatedAt: h.UpdatedAt,
		UpdatedBy: h.UpdatedBy, Kind: h.Kind, Code: h.Code, Name: h.Name, IsStandard: h.IsStandard, RootNodeID: h.RootNodeID}
	for _, n := range t.subtree(top) {
		out.Nodes = append(out.Nodes, gen.HierarchyNode{ID: n.ID, ParentID: apitypes.OptUUID(n.ParentID), Code: n.Code, Name: n.Name,
			Position: n.Position, Level: int32(n.Level), CenterIds: append([]uuid.UUID{}, n.Centers...)})
	}
	return &gen.HierarchyTreeHeaders{ETag: gen.NewOptString(etag.Format(t.Version)), Response: out}
}

// CreateHierarchy implements POST /v1/hierarchies.
func (e *Endpoints) CreateHierarchy(ctx context.Context, req *gen.HierarchyCreate, _ gen.CreateHierarchyParams) (gen.CreateHierarchyRes, error) {
	h, err := CreateHierarchy(ctx, NewHierarchy{Kind: req.Kind, Code: req.Code, Name: req.Name, IsStandard: req.IsStandard.Value})
	if err != nil {
		return nil, err
	}
	return &gen.HierarchyHeaders{ETag: gen.NewOptString(etag.Format(h.Version)), Location: gen.NewOptString("/v1/hierarchies/" + h.ID.String()),
		Response: hierarchyToAPI(h)}, nil
}

var hierarchySort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "h.code", Type: "text"}}}

// ListHierarchies implements GET /v1/hierarchies.
func (e *Endpoints) ListHierarchies(ctx context.Context, params gen.ListHierarchiesParams) (gen.ListHierarchiesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	standard := apitypes.Ptr[bool](params.IsStandard)
	scope := fmt.Sprintf("listHierarchies|%s|%s|%v", params.Kind.Value, params.Code.Value, standard)
	ks, err := e.Pages.Keyset(hierarchySort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(hierarchySelect+` WHERE ($1 = '' OR h.kind = $1) AND ($2 = '' OR h.code = $2) AND ($3::boolean IS NULL OR h.is_standard = $3)`,
		[]any{params.Kind.Value, params.Code.Value, standard})
	rows, _ := tx.Query(ctx, q, args...)
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hierarchy, error) { return scanHierarchy(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list hierarchies: %w", err)
	}
	items, next := apitypes.PageOf(ks, hs, func(h Hierarchy) (string, uuid.UUID) { return h.Code, h.ID }, hierarchyToAPI)
	return &gen.HierarchyPage{Items: items, NextPageToken: next}, nil
}

// GetHierarchy implements GET /v1/hierarchies/{id}.
func (e *Endpoints) GetHierarchy(ctx context.Context, params gen.GetHierarchyParams) (gen.GetHierarchyRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	t, err := visibleTree(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	return treeToAPI(t, t.RootID), nil
}

// GetHierarchyNode implements GET /v1/hierarchies/{id}/nodes/{node_id}.
func (e *Endpoints) GetHierarchyNode(ctx context.Context, params gen.GetHierarchyNodeParams) (gen.GetHierarchyNodeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	t, err := visibleTree(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	if _, ok := t.nodes[params.NodeID]; !ok {
		return nil, problem.NotFoundOf("hierarchy node")
	}
	return treeToAPI(t, params.NodeID), nil
}

// AddHierarchyNode implements POST /v1/hierarchies/{id}/nodes/{node_id}:add.
func (e *Endpoints) AddHierarchyNode(ctx context.Context, req *gen.HierarchyNodeAdd, params gen.AddHierarchyNodeParams) (gen.AddHierarchyNodeRes, error) {
	return changeOK(e.RequestHierarchyChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindNodeCreate, nil,
		hierarchyPayload{ParentID: &params.NodeID, Code: req.Code, Name: req.Name, Position: apitypes.Ptr[int32](req.Position)}, req.Reason))
}

// RenameHierarchyNode implements POST /v1/hierarchies/{id}/nodes/{node_id}:rename.
func (e *Endpoints) RenameHierarchyNode(ctx context.Context, req *gen.HierarchyNodeRename, params gen.RenameHierarchyNodeParams) (gen.RenameHierarchyNodeRes, error) {
	return changeOK(e.RequestHierarchyChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindNodeRename, nil,
		hierarchyPayload{NodeID: &params.NodeID, Name: req.Name}, req.Reason))
}

// MoveHierarchyNode implements POST /v1/hierarchies/{id}/nodes/{node_id}:move.
func (e *Endpoints) MoveHierarchyNode(ctx context.Context, req *gen.HierarchyNodeMove, params gen.MoveHierarchyNodeParams) (gen.MoveHierarchyNodeRes, error) {
	return changeOK(e.RequestHierarchyChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindNodeMove, nil,
		hierarchyPayload{NodeID: &params.NodeID, ParentID: &req.ParentID, Position: apitypes.Ptr[int32](req.Position)}, req.Reason))
}

// RemoveHierarchyNode implements POST /v1/hierarchies/{id}/nodes/{node_id}:remove.
func (e *Endpoints) RemoveHierarchyNode(ctx context.Context, req *gen.HierarchyNodeRemove, params gen.RemoveHierarchyNodeParams) (gen.RemoveHierarchyNodeRes, error) {
	return changeOK(e.RequestHierarchyChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindNodeRemove, nil,
		hierarchyPayload{NodeID: &params.NodeID}, req.Reason))
}

// PlaceInHierarchy implements POST /v1/hierarchies/{id}:place.
func (e *Endpoints) PlaceInHierarchy(ctx context.Context, req *gen.HierarchyPlace, params gen.PlaceInHierarchyParams) (gen.PlaceInHierarchyRes, error) {
	var node *uuid.UUID
	if !req.NodeID.Null {
		node = &req.NodeID.Value
	}
	return changeOK(e.RequestHierarchyChange(ctx, params.ID, etag.IfMatch(params.IfMatch), KindPlace, &req.CenterID,
		hierarchyPayload{NodeID: node}, req.Reason))
}

// pathSort pages a center's paths by the hierarchy's code.
var pathSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// centerPath is a center's place in one hierarchy.
type centerPath struct {
	HierarchyID uuid.UUID
	Code        string
	IsStandard  bool
	NodeID      uuid.UUID
	Nodes       []gen.CenterPathNode
}

// centerPaths lists a page of where a center sits, each path from the
// root to the center's leaf.
func (e *Endpoints) centerPaths(ctx context.Context, tx pgx.Tx, kind string, center uuid.UUID, page pagination.Request) (*gen.CenterPathPage, error) {
	ks, err := e.Pages.Keyset(pathSort, "centerPaths|"+kind+"|"+center.String(), page)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT id, code, is_standard, node_id FROM (SELECT h.id, h.code, h.is_standard, m.node_id FROM ledger.hierarchy_member m
		JOIN ledger.hierarchy h ON h.tenant_id = m.tenant_id AND h.id = m.hierarchy_id
		WHERE m.kind = $1 AND coalesce(m.cost_center_id, m.profit_center_id) = $2 AND m.removed_by_change_id IS NULL) p WHERE true`, []any{kind, center})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (centerPath, error) {
		var p centerPath
		return p, r.Scan(&p.HierarchyID, &p.Code, &p.IsStandard, &p.NodeID)
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: list hierarchies of %s %s: %w", kind, center, err)
	}
	leaves := ids(ps, func(p centerPath) uuid.UUID { return p.NodeID })
	rows, _ = tx.Query(ctx, `WITH RECURSIVE up (leaf, id, parent_id, code, name, depth) AS (
			SELECT n.id, n.id, n.parent_id, n.code, n.name, 0 FROM ledger.hierarchy_node n WHERE n.id = ANY($1)
			UNION ALL
			SELECT up.leaf, n.id, n.parent_id, n.code, n.name, up.depth + 1 FROM up JOIN ledger.hierarchy_node n ON n.id = up.parent_id)
		SELECT leaf, id, code, name FROM up ORDER BY leaf, depth DESC`, leaves)
	paths := map[uuid.UUID][]gen.CenterPathNode{}
	var leaf uuid.UUID
	var n gen.CenterPathNode
	if _, err := pgx.ForEachRow(rows, []any{&leaf, &n.ID, &n.Code, &n.Name}, func() error {
		paths[leaf] = append(paths[leaf], n)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: read paths of %s %s: %w", kind, center, err)
	}
	items, next := apitypes.PageOf(ks, ps, func(p centerPath) (string, uuid.UUID) { return p.Code, p.HierarchyID }, func(p centerPath) gen.CenterPath {
		return gen.CenterPath{HierarchyID: p.HierarchyID, HierarchyCode: p.Code, IsStandard: p.IsStandard, Nodes: paths[p.NodeID]}
	})
	return &gen.CenterPathPage{Items: items, NextPageToken: next}, nil
}

// ListCostCenterPaths implements GET /v1/cost-centers/{id}/hierarchy-paths.
func (e *Endpoints) ListCostCenterPaths(ctx context.Context, params gen.ListCostCenterPathsParams) (gen.ListCostCenterPathsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := getCostCenter(ctx, tx, access, params.ID, false); err != nil {
		return nil, err
	}
	return e.centerPaths(ctx, tx, HierarchyCostCenters, params.ID, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
}

// ListProfitCenterPaths implements GET /v1/profit-centers/{id}/hierarchy-paths.
func (e *Endpoints) ListProfitCenterPaths(ctx context.Context, params gen.ListProfitCenterPathsParams) (gen.ListProfitCenterPathsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := getProfitCenter(ctx, tx, params.ID, noLock); err != nil {
		return nil, err
	}
	return e.centerPaths(ctx, tx, HierarchyProfitCenters, params.ID, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
}
