package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The cash context's configuration (T53b, migrations 000141-000143): a
// liquidity item's code, name and direction, its parent another item of
// the tenant, never itself; a planning group's code and name; a supplier's
// planning group a code of that form. Items and groups are deactivated,
// never deleted, and change by versions.
func TestCashConfigurationRules(t *testing.T) {
	need(t)
	const (
		item  = `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $6)`
		group = `INSERT INTO cash.planning_group (tenant_id, code, name, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`
		set   = `INSERT INTO partner.supplier_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code,
			planning_group, created_by, updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $6, $6)`
	)
	top := func(f fixture) stmt { return step(item, f.tenant, "OPERATING", "Operating", "both", nil, f.actor) }
	for _, c := range []struct {
		name string
		run  func(tx pgx.Tx, f fixture) error
		want string // empty: allowed
	}{
		{"a tree of two items", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(item, f.tenant, "OP_RENT", "Rent", "outflow", "OPERATING", f.actor))
		}, ""},
		{"a child before its parent in one statement", func(tx pgx.Tx, f fixture) error {
			return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by)
				VALUES ($1, 'OP_RENT', 'Rent', 'outflow', 'OPERATING', $2, $2), ($1, 'OPERATING', 'Operating', 'both', NULL, $2, $2)`, f.tenant, f.actor)
		}, ""},
		{"an item renamed and deactivated", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`UPDATE cash.liquidity_item SET name = 'Ops', is_active = false, version = version + 1 WHERE code = 'OPERATING'`))
		}, ""},
		{"an item changed without its version", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`UPDATE cash.liquidity_item SET name = 'Ops' WHERE code = 'OPERATING'`))
		}, "optimistic lock"},
		{"an item deleted", func(tx pgx.Tx, f fixture) error {
			return all(tx, top(f), step(`DELETE FROM cash.liquidity_item WHERE code = 'OPERATING'`))
		}, "permission denied"},
		{"an item code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "op", "Operating", "both", nil, f.actor)
		}, "liquidity_item_code_check"},
		{"an item without a name", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", " ", "both", nil, f.actor)
		}, "liquidity_item_name_check"},
		{"an item's direction", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", "Operating", "out", nil, f.actor)
		}, "liquidity_item_direction_check"},
		{"an item its own parent", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OPERATING", "Operating", "both", "OPERATING", f.actor)
		}, "liquidity_item_check"},
		{"an unknown parent", func(tx pgx.Tx, f fixture) error {
			return exec(tx, item, f.tenant, "OP_RENT", "Rent", "outflow", "OPERATING", f.actor)
		}, "liquidity_item_tenant_id_parent_code_fkey"},
		{"a planning group", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "TAX_AUTHORITY", "Tax authorities", f.actor)
		}, ""},
		{"a planning group code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "_TAX", "Tax authorities", f.actor)
		}, "planning_group_code_check"},
		{"a planning group without a name", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, "TAX", "", f.actor)
		}, "planning_group_name_check"},
		{"a planning group deleted", func(tx pgx.Tx, f fixture) error {
			return all(tx, step(group, f.tenant, "TAX", "Tax", f.actor), step(`DELETE FROM cash.planning_group WHERE code = 'TAX'`))
		}, "permission denied"},
		{"a supplier's planning group", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, "TAX_AUTHORITY", f.actor)
		}, ""},
		{"a supplier without one", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, nil, f.actor)
		}, ""},
		{"a supplier's planning group code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, set, f.tenant, f.supplier, f.entity, f.ap, "tax", f.actor)
		}, "supplier_company_planning_group"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return c.run(tx, f) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// A liquidity item's parent is the tenant's own: another tenant's item of
// the code is not one.
func TestCashParentIsTheTenants(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	mustApp(t, a.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, created_by, updated_by) VALUES ($1, 'OPERATING', 'Operating', 'both', $2, $2)`,
			a.tenant, a.actor)
	})
	err := asApp(t, b.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO cash.liquidity_item (tenant_id, code, name, direction, parent_code, created_by, updated_by)
			VALUES ($1, 'OP_RENT', 'Rent', 'outflow', 'OPERATING', $2, $2)`, b.tenant, b.actor)
	})
	failsWith(t, err, "liquidity_item_tenant_id_parent_code_fkey")
}

// TestCashPositionPermission: the cash position's read permission is
// seeded and is no approval permission (T53c, 000149).
func TestCashPositionPermission(t *testing.T) {
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'cash.position.read'`).Scan(&approval); err != nil {
		t.Fatalf("cash.position.read: %v", err)
	}
	if approval {
		t.Error("cash.position.read: got an approval permission, want a read permission")
	}
}
