package db

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Hierarchies of cost and profit centers (T56b, migration 000139): the
// rules the database keeps for trees, places and the changes that write
// them.

// chain inserts nodes below parent, each the child of the one before,
// and returns their ids.
func (f centerFixture) chain(t *testing.T, tree, parent uuid.UUID, prefix string, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, n)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for i := range ids {
			ids[i] = id()
			if err := exec(tx, node, f.tenant, ids[i], tree, parent, fmt.Sprintf("%s%d", prefix, i), f.actor); err != nil {
				return err
			}
			parent = ids[i]
		}
		return nil
	})
	return ids
}

// alternative inserts an alternative cost center hierarchy with its root.
func (f centerFixture) alternative(t *testing.T, code string) (tree, root uuid.UUID) {
	t.Helper()
	tree, root = id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, hierarchy, f.tenant, tree, "cost_center", code, false, f.actor); err != nil {
			return err
		}
		return exec(tx, node, f.tenant, root, tree, nil, code, f.actor)
	})
	return tree, root
}

const moveNode = `UPDATE ledger.hierarchy_node SET parent_id = $2, version = version + 1 WHERE id = $1`

func TestHierarchies(t *testing.T) {
	need(t)
	f := newCenters(t)
	alt, altRoot := f.alternative(t, "ALT")
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a second standard hierarchy of a kind": {hierarchy, []any{f.tenant, id(), "cost_center", "STD2", true, f.actor}, "hierarchy_one_standard"},
		"a hierarchy code twice in a kind":      {hierarchy, []any{f.tenant, id(), "cost_center", "ALT", false, f.actor}, "hierarchy_code_once"},
		"a hierarchy of another kind":           {hierarchy, []any{f.tenant, id(), "order", "X", false, f.actor}, "hierarchy_kind_check"},
		"a hierarchy's standing changed": {`UPDATE ledger.hierarchy SET is_standard = true, version = version + 1 WHERE id = $1`, []any{alt},
			"are fixed"},
		"a hierarchy's code changed":       {`UPDATE ledger.hierarchy SET code = 'ALT2', version = version + 1 WHERE id = $1`, []any{alt}, "are fixed"},
		"a second root":                    {node, []any{f.tenant, id(), alt, nil, "ROOT2", f.actor}, "hierarchy_one_root"},
		"a hierarchy code SAP cannot hold": {hierarchy, []any{f.tenant, id(), "cost_center", "lower", false, f.actor}, "hierarchy_code_check"},
		"a node code SAP cannot hold":      {node, []any{f.tenant, id(), alt, altRoot, "lower", f.actor}, "hierarchy_node_code_check"},
		"a hierarchy without a name": {`INSERT INTO ledger.hierarchy (tenant_id, id, kind, code, name, is_standard, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'cost_center', 'NONAME', '', false, $2, $2)`, []any{f.tenant, f.actor}, "hierarchy_name_check"},
		"a node without a name": {`UPDATE ledger.hierarchy_node SET name = '', version = version + 1 WHERE id = $1`, []any{f.ccLeaf},
			"hierarchy_node_name_check"},
		"a negative position": {`UPDATE ledger.hierarchy_node SET position = -1, version = version + 1 WHERE id = $1`, []any{f.ccLeaf},
			"hierarchy_node_position_check"},
		"a parent in another hierarchy": {node, []any{f.tenant, id(), alt, f.ccRoot, "X", f.actor},
			"hierarchy_node_parent"},
		"a node code twice in a hierarchy": {node, []any{f.tenant, id(), f.ccTree, f.ccRoot, "LEAF", f.actor}, "hierarchy_node_code_once"},
		"a node under a node holding centers": {node, []any{f.tenant, id(), f.ccTree, f.ccLeaf, "UNDER", f.actor},
			"the parent node holds centers"},
		"the root moved below a node": {moveNode, []any{altRoot, f.ccLeaf}, "a root stays the root"},
		"a node made a root": {`UPDATE ledger.hierarchy_node SET parent_id = NULL, version = version + 1 WHERE id = $1`, []any{f.ccLeaf},
			"a root stays the root"},
		"a node moved to another hierarchy": {`UPDATE ledger.hierarchy_node SET hierarchy_id = $2, parent_id = $3, version = version + 1 WHERE id = $1`,
			[]any{f.ccLeaf, alt, altRoot}, "a node stays in its hierarchy"},
		"the root removed": {`UPDATE ledger.hierarchy_node SET removed_by_change_id = $2, version = version + 1 WHERE id = $1`, []any{altRoot, f.ccChange},
			"hierarchy_node_root_kept"},
		"a node holding centers removed": {`UPDATE ledger.hierarchy_node SET removed_by_change_id = $2, version = version + 1 WHERE id = $1`,
			[]any{f.ccLeaf, f.ccChange}, "a node with child nodes or centers cannot be removed"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
}

func TestHierarchyNodeMovedBelowCenters(t *testing.T) {
	need(t)
	f := newCenters(t)
	moved := f.chain(t, f.ccTree, f.ccRoot, "M", 1)[0]
	failsWith(t, f.try(t, moveNode, moved, f.ccLeaf), "the parent node holds centers")
}

func TestHierarchyCyclesAndDepth(t *testing.T) {
	need(t)
	f := newCenters(t)
	tree, root := f.alternative(t, "ALT")
	// Root, then 11 levels: 12 in all, the most a hierarchy has.
	deep := f.chain(t, tree, root, "D", 11)
	t.Run("a node its own parent", func(t *testing.T) {
		failsWith(t, f.try(t, moveNode, deep[3], deep[3]), "its own ancestor")
	})
	t.Run("a cycle through a descendant", func(t *testing.T) {
		failsWith(t, f.try(t, moveNode, deep[0], deep[5]), "its own ancestor")
	})
	t.Run("a thirteenth level", func(t *testing.T) {
		failsWith(t, f.try(t, node, f.tenant, id(), tree, deep[10], "D11", f.actor), "at most 12 levels")
	})
	t.Run("a subtree moved too deep", func(t *testing.T) {
		pair := f.chain(t, tree, root, "P", 2)
		failsWith(t, f.try(t, moveNode, pair[0], deep[9]), "at most 12 levels")
		if err := f.try(t, moveNode, pair[1], deep[9]); err != nil {
			t.Errorf("a leaf moved to the twelfth level: got %v, want none", err)
		}
	})
	t.Run("removing a node with children, then the leaf, then the parent", func(t *testing.T) {
		pair := f.chain(t, tree, root, "R", 2)
		remove := `UPDATE ledger.hierarchy_node SET removed_by_change_id = $2, version = version + 1 WHERE id = $1`
		failsWith(t, f.try(t, remove, pair[0], f.ccChange), "a node with child nodes or centers cannot be removed")
		if err := f.try(t, remove, pair[1], f.ccChange); err != nil {
			t.Fatalf("removing a leaf: got %v, want none", err)
		}
		failsWith(t, f.try(t, `UPDATE ledger.hierarchy_node SET name = 'X', version = version + 1 WHERE id = $1`, pair[1]), "a removed node is fixed")
		failsWith(t, f.try(t, node, f.tenant, id(), tree, pair[1], "UNDERGONE", f.actor), "the parent node is removed")
		if err := f.try(t, node, f.tenant, id(), tree, pair[0], "R1", f.actor); err != nil {
			t.Errorf("a removed node's code used again: got %v, want none", err)
		}
	})
}

func TestHierarchyMembers(t *testing.T) {
	need(t)
	f := newCenters(t)
	alt, altRoot := f.alternative(t, "ALT")
	leaf := f.chain(t, alt, altRoot, "L", 1)[0]
	removed := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, node, f.tenant, removed, alt, altRoot, "GONE", f.actor); err != nil {
			return err
		}
		return exec(tx, `UPDATE ledger.hierarchy_node SET removed_by_change_id = $2, version = version + 1 WHERE id = $1`, removed, f.ccChange)
	})
	for name, c := range map[string]struct {
		args []any
		want string
	}{
		"a center at a node with children": {[]any{f.tenant, alt, "cost_center", altRoot, f.cc, nil, f.actor}, "sits at a leaf node"},
		"a center at a removed node":       {[]any{f.tenant, alt, "cost_center", removed, f.cc, nil, f.actor}, "sits at a leaf node"},
		"a node of another hierarchy":      {[]any{f.tenant, alt, "cost_center", f.ccLeaf, f.cc, nil, f.actor}, "hierarchy_member_node"},
		"a profit center in a cost center hierarchy": {[]any{f.tenant, alt, "profit_center", leaf, nil, f.pc, f.actor},
			"hierarchy_member_kind"},
		"a kind its center does not match": {[]any{f.tenant, alt, "cost_center", leaf, nil, f.pc, f.actor}, "hierarchy_member_center"},
		"a center twice in a hierarchy":    {[]any{f.tenant, f.ccTree, "cost_center", f.ccLeaf, f.cc, nil, f.actor}, "hierarchy_member_once"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, member, c.args...), c.want)
		})
	}
	if err := f.try(t, member, f.tenant, alt, "cost_center", leaf, f.cc, nil, f.actor); err != nil {
		t.Fatalf("a center in an alternative hierarchy: got %v, want none", err)
	}
	t.Run("a place's center changed", func(t *testing.T) {
		failsWith(t, f.try(t, `UPDATE ledger.hierarchy_member SET cost_center_id = $2, version = version + 1 WHERE node_id = $1`, leaf, f.rejected),
			"a place changes only its node")
	})
	t.Run("removed from an alternative hierarchy, then fixed", func(t *testing.T) {
		if err := f.try(t, `UPDATE ledger.hierarchy_member SET removed_by_change_id = $2, version = version + 1 WHERE node_id = $1`, leaf, f.ccChange); err != nil {
			t.Fatalf("removing a place in an alternative hierarchy: got %v, want none", err)
		}
		failsWith(t, f.try(t, `UPDATE ledger.hierarchy_member SET node_id = $2, version = version + 1 WHERE node_id = $1`, leaf, leaf),
			"a place changes only its node")
	})
}

func TestCentersInTheStandardHierarchy(t *testing.T) {
	need(t)
	f := newCenters(t)
	t.Run("an active center removed from the standard hierarchy", func(t *testing.T) {
		failsWith(t, f.try(t, `UPDATE ledger.hierarchy_member SET removed_by_change_id = $2, version = version + 1 WHERE cost_center_id = $1 AND hierarchy_id = $3`,
			f.cc, f.ccChange, f.ccTree), "an active center sits in the standard hierarchy")
	})
	t.Run("an active profit center created outside it", func(t *testing.T) {
		failsWith(t, f.try(t, `INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'PC2', 'P', 'J', 'active', $2, $2)`, f.tenant, f.actor), "an active center sits in the standard hierarchy")
	})
	t.Run("a pending center activated outside it, then placed", func(t *testing.T) {
		pending := id()
		insert := `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, $2, $3, 'P1', 'CC', 'J', 'pending', $4, $4)`
		activate := `UPDATE ledger.cost_center SET status = 'active', version = version + 1 WHERE id = $1`
		if err := f.try(t, insert, f.tenant, pending, f.entity, f.actor); err != nil {
			t.Fatalf("a pending center outside the hierarchy: got %v, want none", err)
		}
		failsWith(t, f.try(t, activate, pending), "an active center sits in the standard hierarchy")
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, activate, pending); err != nil {
				return err
			}
			return exec(tx, member, f.tenant, f.ccTree, "cost_center", f.ccLeaf, pending, nil, f.actor)
		})
	})
	t.Run("moved within the standard hierarchy", func(t *testing.T) {
		other := f.chain(t, f.ccTree, f.ccRoot, "O", 1)[0]
		if err := f.try(t, `UPDATE ledger.hierarchy_member SET node_id = $2, version = version + 1 WHERE cost_center_id = $1 AND hierarchy_id = $3`,
			f.cc, other, f.ccTree); err != nil {
			t.Errorf("a move to another leaf: got %v, want none", err)
		}
	})
}

func TestHierarchyChanges(t *testing.T) {
	need(t)
	f := newCenters(t)
	change := `INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, hierarchy_id, kind, payload, reason, status, requested_by, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '{}', 'r', 'pending', $6, $6, $6)`
	for name, c := range map[string]struct {
		cc, tree *uuid.UUID
		kind     string
		want     string
	}{
		"a node change naming a center":      {&f.cc, &f.ccTree, "node_create", "center_change_one_center"},
		"a node change without a hierarchy":  {nil, nil, "node_move", "center_change_hierarchy"},
		"a place without a hierarchy":        {&f.cc, nil, "place", "center_change_hierarchy"},
		"a place without a center":           {nil, &f.ccTree, "place", "center_change_one_center"},
		"a center change naming a hierarchy": {&f.cc, &f.ccTree, "lock", "center_change_hierarchy"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, change, f.tenant, id(), c.cc, c.tree, c.kind, f.actor), c.want)
		})
	}
	pending := id()
	if err := f.try(t, change, f.tenant, pending, nil, f.ccTree, "node_remove", f.actor); err != nil {
		t.Fatalf("a node change: got %v, want none", err)
	}
	t.Run("its hierarchy changed", func(t *testing.T) {
		failsWith(t, f.try(t, `UPDATE ledger.center_change SET hierarchy_id = $2, version = version + 1 WHERE id = $1`, pending, f.pcTree),
			"what a change asks for is fixed")
	})
	t.Run("node changes do not count as a center's pending change", func(t *testing.T) {
		if err := f.try(t, change, f.tenant, id(), nil, f.ccTree, "node_rename", f.actor); err != nil {
			t.Errorf("a second pending node change: got %v, want none", err)
		}
	})
}

// TestHierarchyBackfill: centers that exist before migration 000139 are
// placed in a STANDARD hierarchy's UNASSIGNED leaf when active; pending and
// rejected ones are not.
func TestHierarchyBackfill(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	all := Migrations()
	at := slices.IndexFunc(all, func(m Migration) bool { return strings.HasSuffix(m.Name, "_hierarchies") })
	if _, err := migrate(ctx, conn, all[:at]); err != nil {
		t.Fatal(err)
	}
	tenant, actor := id(), id()
	insert := map[string]string{
		"cost_center": `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $1, $3, 'N', 'J', $4, $2, $2)`,
		"profit_center": `INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $3, 'N', 'J', $4, $2, $2)`,
	}
	for _, c := range []struct{ table, code, status string }{
		{"cost_center", "ACTIVE", "active"}, {"cost_center", "PENDING", "pending"}, {"cost_center", "REJECTED", "rejected"},
		{"profit_center", "PC", "active"},
	} {
		if _, err := conn.Exec(ctx, insert[c.table], tenant, actor, c.code, c.status); err != nil {
			t.Fatalf("%s %s: %v", c.table, c.code, err)
		}
	}
	if _, err := migrate(ctx, conn, all[at:]); err != nil {
		t.Fatal(err)
	}
	rows, _ := conn.Query(ctx, `SELECT h.kind || ' ' || h.code || '/' || p.code || '/' || n.code || ' ' || coalesce(cc.code, pc.code)
		FROM ledger.hierarchy_member m JOIN ledger.hierarchy h ON h.id = m.hierarchy_id AND h.is_standard
		JOIN ledger.hierarchy_node n ON n.id = m.node_id JOIN ledger.hierarchy_node p ON p.id = n.parent_id
		LEFT JOIN ledger.cost_center cc ON cc.id = m.cost_center_id LEFT JOIN ledger.profit_center pc ON pc.id = m.profit_center_id
		WHERE m.tenant_id = $1 ORDER BY 1`, tenant)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cost_center STANDARD/STANDARD/UNASSIGNED ACTIVE", "profit_center STANDARD/STANDARD/UNASSIGNED PC"}
	if !slices.Equal(got, want) {
		t.Errorf("places after the backfill: got %v, want %v", got, want)
	}
}
