package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestMovementTypesOfCountAndShipping (S2d's migrations 000407 and 000408,
// S2b1's 000410 and 000411, which run after them; batch 83): after the last
// migration a material document takes the goods issue's 601 and its
// reversal 602 beside the count's 701 and 702, and the account
// determination both the inventory_difference and the
// inventory_cost_of_goods_sold keys. Reverting 000411 and 000410 restores
// 000408's checks: 701, 702 and inventory_difference:<class> still taken,
// 601, 602 and inventory_cost_of_goods_sold:<class> refused.
func TestMovementTypesOfCountAndShipping(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// clear takes the goods issues (this test's and those other tests
	// committed) out of the way of 000410's down, which refuses to revert
	// while any exists; rolled back with the rest.
	clear := func(t *testing.T) {
		t.Helper()
		if err := all(tx, step("DELETE FROM inventory.material_document WHERE movement_type IN ('601', '602')"),
			step("DELETE FROM ledger.account_determination WHERE key LIKE 'inventory_cost_of_goods_sold:%'")); err != nil {
			t.Fatal(err)
		}
	}
	// Foreign keys and triggers aside: the checks are what is tested.
	if err := exec(tx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	tenant := id()
	check := func(t *testing.T, stage string, want map[string]string) {
		t.Helper()
		issue := id()
		for _, c := range []struct {
			name, sql string
			args      []any
		}{
			{"601", movementTypeDoc, []any{tenant, issue, "601", nil, "OD1"}},
			{"602", movementTypeDoc, []any{tenant, id(), "602", issue, "OD1"}},
			{"701", movementTypeDoc, []any{tenant, id(), "701", nil, nil}},
			{"702", movementTypeDoc, []any{tenant, id(), "702", nil, nil}},
			{"inventory_difference:3000", movementTypeKey, []any{id(), "inventory_difference:3000"}},
			{"inventory_cost_of_goods_sold:3000", movementTypeKey, []any{id(), "inventory_cost_of_goods_sold:3000"}},
		} {
			err := savepoint(tx, c.sql, c.args...)
			if w := want[c.name]; w == "" {
				if err != nil {
					t.Errorf("%s, %s: got %v, want it taken", stage, c.name, err)
				}
			} else if !violates(err, w) {
				t.Errorf("%s, %s: got %v, want a violation of %s", stage, c.name, err, w)
			}
		}
	}
	check(t, "after the last migration", nil)
	clear(t)
	for _, name := range []string{"000411_validate_goods_issue", "000410_goods_issue"} {
		if err := exec(tx, migrationNamed(t, name).Down); err != nil {
			t.Fatalf("%s's down: %v", name, err)
		}
	}
	check(t, "after reverting 000411 and 000410", map[string]string{"601": "material_document_movement_type_check",
		"602": "material_document_movement_type_check", "inventory_cost_of_goods_sold:3000": "account_determination_key_check"})
}

const (
	movementTypeDoc = `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
		reversal_of_id, reference, created_by) VALUES ($1, $2, gen_random_uuid(), 2026, gen_random_uuid()::text, $3, '2026-11-20', $4, $5, $1)`
	movementTypeKey = `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $1, $2, $1, $1)`
)

// savepoint runs one statement under a savepoint, so a refused one leaves
// the transaction usable.
func savepoint(tx pgx.Tx, sql string, args ...any) error {
	if err := exec(tx, "SAVEPOINT one"); err != nil {
		return err
	}
	if err := exec(tx, sql, args...); err != nil {
		if rerr := exec(tx, "ROLLBACK TO SAVEPOINT one"); rerr != nil {
			return rerr
		}
		return err
	}
	return exec(tx, "RELEASE SAVEPOINT one")
}

// violates says whether err is the database refusing a row by the
// constraint named.
func violates(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.ConstraintName == constraint
}
