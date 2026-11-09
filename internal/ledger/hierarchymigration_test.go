package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// migratedTrees loads SAP-like groups as a run does: H1 > H2 below the
// standard hierarchy's root (created, as the tenant has none), a cost
// center created at H2, and an alternative hierarchy ALT with leaf A1 at
// which the cost center is placed.
func (c centers) migratedTrees(t *testing.T) (std, alt, h2, a1 uuid.UUID, cc string) {
	t.Helper()
	h1, a1, h2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		var root uuid.UUID
		var err error
		if std, root, err = m.StandardHierarchy(ctx, ledger.HierarchyCostCenters); err != nil {
			return err
		}
		if err := m.AddNodes(ctx, std, []ledger.MigratedNode{{ID: h1, Parent: root, Code: "H1", Name: "Group"}, {ID: h2, Parent: h1, Code: "H2", Name: "Admin"}},
			"Migration run x: SAP cost_center group 0101/A1/H1"); err != nil {
			return err
		}
		id, err := m.CreateCostCenter(ctx, ledger.MigratedCostCenter{LegalEntityID: c.de, Code: "C1", Texts: ledger.Texts{Name: "C 1", Responsible: "R"},
			StandardNodeID: &h2, Reason: "r", Periods: []ledger.MigratedPeriod[ledger.CostCenterAttrs]{{From: day("2026-01-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN"}}}})
		if err != nil {
			return err
		}
		cc = id.String()
		if alt, root, err = m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyCostCenters, Code: "ALT", Name: "Regions"}); err != nil {
			return err
		}
		if err := m.AddNodes(ctx, alt, []ledger.MigratedNode{{ID: a1, Parent: root, Code: "A1", Name: "East"}}, "r"); err != nil {
			return err
		}
		return m.PlaceCenters(ctx, alt, []ledger.MigratedPlace{{Center: id, Node: a1}}, "Migration run x: SAP cost_center group 0101/A1/ALT")
	}); err != nil {
		t.Fatal(err)
	}
	return std, alt, h2, a1, cc
}

// TestHierarchyMigration: a run's groups are nodes added and centers placed
// by applied changes without an approval request (ADR-0050 §10, T56h2),
// SAP's standard hierarchy below the standard hierarchy's root, which the
// tenant gets when it has none, and the center at its leaf there, placed
// by its create change.
func TestHierarchyMigration(t *testing.T) {
	c := setupBareCenters(t)
	std, alt, _, _, cc := c.migratedTrees(t)
	if got, want := fmt.Sprint(c.paths(t, cc)), "[ALT:ALT/A1 STANDARD:STANDARD/H1/H2]"; got != want {
		t.Errorf("the cost center's paths: got %s, want %s", got, want)
	}
	var again uuid.UUID
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		var err error
		again, _, err = m.StandardHierarchy(ctx, ledger.HierarchyCostCenters)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again != std {
		t.Errorf("the standard hierarchy again: got %s, want %s", again, std)
	}
	page, _ := c.Must(200, "GET", "/v1/center-changes?hierarchy_id="+std.String(), "", "")
	var kinds []string
	for _, it := range page["items"].([]any) {
		ch := it.(map[string]any)
		kinds = append(kinds, fmt.Sprintf("%v/%v/%v", ch["kind"], ch["status"], ch["workflow_request_id"]))
	}
	if got, want := fmt.Sprint(kinds), "[node_create/applied/<nil> node_create/applied/<nil>]"; got != want {
		t.Errorf("the standard hierarchy's changes: got %s, want %s", got, want)
	}
	page, _ = c.Must(200, "GET", "/v1/center-changes?hierarchy_id="+alt.String(), "", "")
	if n := len(page["items"].([]any)); n != 2 {
		t.Errorf("the alternative hierarchy's changes: got %d, want a node and a place", n)
	}
}

// TestHierarchyMigrationRefusals: nodes and places are planned as an
// approved change is.
func TestHierarchyMigrationRefusals(t *testing.T) {
	c := setupBareCenters(t)
	std, alt, h2, a1, cc := c.migratedTrees(t)
	center := uuid.MustParse(cc)
	for name, tc := range map[string]struct {
		call func(ctx context.Context, m ledger.CenterMigration) error
		want string
	}{
		"a code twice": {func(ctx context.Context, m ledger.CenterMigration) error {
			h, root, err := m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyCostCenters, Code: "ALT4", Name: "N"})
			if err != nil {
				return err
			}
			return m.AddNodes(ctx, h, []ledger.MigratedNode{{ID: uuid.New(), Parent: root, Code: "N1", Name: "N"}, {ID: uuid.New(), Parent: root, Code: "N1", Name: "N"}}, "r")
		}, "ledger.hierarchy_node_exists"},
		"an unknown hierarchy": {func(ctx context.Context, m ledger.CenterMigration) error {
			return m.AddNodes(ctx, uuid.New(), nil, "r")
		}, "request.not_found"},
		"placed twice": {func(ctx context.Context, m ledger.CenterMigration) error {
			return m.PlaceCenters(ctx, alt, []ledger.MigratedPlace{{Center: center, Node: a1}}, "r")
		}, "ledger.nothing_to_change"},
		"an unknown hierarchy to place in": {func(ctx context.Context, m ledger.CenterMigration) error {
			return m.PlaceCenters(ctx, uuid.New(), nil, "r")
		}, "request.not_found"},
		"a hierarchy code taken": {func(ctx context.Context, m ledger.CenterMigration) error {
			_, _, err := m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyCostCenters, Code: "ALT", Name: "N"})
			return err
		}, "ledger.hierarchy_exists"},
		"STANDARD taken": {func(ctx context.Context, m ledger.CenterMigration) error {
			if _, _, err := m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyProfitCenters, Code: "STANDARD", Name: "N"}); err != nil {
				return err
			}
			_, _, err := m.StandardHierarchy(ctx, ledger.HierarchyProfitCenters)
			return err
		}, "ledger.hierarchy_exists"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := problem.From(c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error { return tc.call(ctx, m) })).Code; got != tc.want {
				t.Errorf("got %q, want %s", got, tc.want)
			}
		})
	}
	err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		return m.AddNodes(ctx, std, []ledger.MigratedNode{{ID: uuid.New(), Parent: h2, Code: "H3", Name: "N"}}, "r")
	})
	if err != nil {
		t.Errorf("below a node holding centers (A10(d)): got %v, want none", err)
	}
}

// TestHierarchyMigrationDatabaseFailures runs each call outside a unit of
// work and with each of its statements failing in turn.
func TestHierarchyMigrationDatabaseFailures(t *testing.T) {
	c := setupBareCenters(t)
	_, alt, h2, a1, _ := c.migratedTrees(t)
	m := ledger.CenterMigration{}
	var other uuid.UUID
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		var err error
		other, err = m.CreateCostCenter(ctx, ledger.MigratedCostCenter{LegalEntityID: c.de, Code: "C2", Texts: ledger.Texts{Name: "C 2", Responsible: "R"},
			StandardNodeID: &h2, Reason: "r", Periods: []ledger.MigratedPeriod[ledger.CostCenterAttrs]{{From: day("2026-01-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN"}}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddNodes(context.Background(), alt, nil, "r"); err == nil {
		t.Error("add nodes outside a unit of work: got no error, want one")
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"standard hierarchy": func(ctx context.Context) error {
			_, _, err := m.StandardHierarchy(ctx, ledger.HierarchyProfitCenters)
			return err
		},
		"create hierarchy": func(ctx context.Context) error {
			_, _, err := m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyCostCenters, Code: "ALT2", Name: "N"})
			return err
		},
		"add nodes": func(ctx context.Context) error {
			h, root, err := m.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: ledger.HierarchyCostCenters, Code: "ALT3", Name: "N"})
			if err != nil {
				return err
			}
			return m.AddNodes(ctx, h, []ledger.MigratedNode{{ID: uuid.New(), Parent: root, Code: "A2", Name: "N"}}, "r")
		},
		"place centers": func(ctx context.Context) error {
			return m.PlaceCenters(ctx, alt, []ledger.MigratedPlace{{Center: other, Node: a1}}, "r")
		},
		"standard hierarchy found": func(ctx context.Context) error {
			_, _, err := m.StandardHierarchy(ctx, ledger.HierarchyCostCenters)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, c.ten, c.ten.Human, name, call)
		})
	}
}
