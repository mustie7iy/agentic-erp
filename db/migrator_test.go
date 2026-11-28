package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// asMigrator sets the transaction's role to the one 06 §6.4 guards
// against: a migrating login that does not bypass row-level security
// (neither superuser nor BYPASSRLS), a member of erp_owner, with usage and
// create on every schema, as the schemas' owner has. The role is made in
// the transaction and goes with it; its name is random, since roles are
// the cluster's and other runs make theirs alongside.
func asMigrator(tx pgx.Tx) error {
	role := pgx.Identifier{"erp_test_migrator_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	rows, _ := tx.Query(context.Background(), `SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY nspname`)
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for i, s := range schemas {
		schemas[i] = pgx.Identifier{s}.Sanitize()
	}
	return all(tx, step(`CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS IN ROLE erp_owner`),
		step(`GRANT USAGE, CREATE ON SCHEMA `+strings.Join(schemas, ", ")+` TO `+role), step(`SET LOCAL ROLE `+role))
}

// downRefused asserts that a down migration refuses the rows written in
// the tenant (rather than dropping them, 06 §6.4): as a migrating role
// held to row-level security its guard fails on that security, not on
// seeing no rows, and as the owner it refuses naming want. Nothing is
// kept: the transaction is rolled back. Down tests written before it
// (000471, 000473, 000475) keep their own steps.
func downRefused(t *testing.T, down string, tenant uuid.UUID, rows []stmt, want string) {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	written := append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String())}, rows...)
	if err := all(tx, append(written, step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`))...); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), want)
}

// TestDownsRevert (batch 95S): on a database holding nothing the guards
// refuse, the downs of 000486 (lot sizing) and of 000485 and 000484
// (material descriptions), batch 96S's of 000490 (recounts), 000491
// (goods issue lines), 000493 and 000492 (lead times), batch 97S's of
// 000494 (S&OP approval), 000495 (units' measures) and 000496 (indicators
// taken off, ABC analyses applied), batch 98F's of 000498 and 000497
// (the house bank code per account), 000499 (opening items) and 000501 and
// 000500 (the forecast's receipts, T53l1), and batch 98S's of 000503 and
// 000502 (returns to the supplier) and of 000505 and 000504 (customer
// returns into stock) revert what their ups added, so that each up
// applies again after its down (06 §6.4: a down reverts its up, and
// TestMigrationsRevertCleanly, reverting the whole chain, would not see
// one that left its columns behind).
func TestDownsRevert(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	if _, err := Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		names                 []string // the migrations reverted, latest first
		schema, table, column string   // a column the first up added
		// check, for an up that adds no column, is true while what it
		// changed is still there; schema, table and column then only name
		// the case in a failure.
		check string
	}{
		{[]string{"000496_cycle_count_removal"}, "inventory", "cycle_count_material", "status", ""},
		{[]string{"000495_material_measures"}, "materials", "unit_measure", "gtin", ""},
		{[]string{"000494_sop_plan_approval"}, "mrp", "sop_plan", "approval_request_id", ""},
		{[]string{"000486_mrp_lot_sizing"}, "mrp", "material_plan", "lot_size", ""},
		{[]string{"000490_count_recounts"}, "inventory", "count_item", "recounts", ""},
		{[]string{"000491_delivery_issue_tracking"}, "shipping", "outbound_delivery_issue_line", "batch_id", ""},
		{[]string{"000493_sales_lead_time"}, "sales", "sales_order_item", "lead_time_quantity", ""},
		{[]string{"000492_mrp_lead_time"}, "mrp", "material_plan", "replenishment_lead_time_days", ""},
		{[]string{"000485_validate_material_descriptions", "000484_material_descriptions"}, "materials", "material", "description_language", ""},
		{names: []string{"000498_house_bank_code_per_account_validate", "000497_house_bank_code_per_account"}, schema: "payments", table: "house_bank", column: "code",
			check: `SELECT EXISTS (SELECT FROM pg_constraint WHERE conrelid = 'payments.house_bank'::regclass AND conname = 'house_bank_code_check'
				AND pg_get_constraintdef(oid) LIKE '%{1,10}%')`},
		{[]string{"000499_opening_item"}, "integration", "opening_item", "external_key", ""},
		{[]string{"000501_validate_cash_receipts", "000500_cash_receipts"}, "cash", "snapshot_flow", "customer_id", ""},
		{names: []string{"000503_validate_supplier_returns", "000502_supplier_returns"}, schema: "inventory", table: "material_document", column: "movement_type",
			check: `SELECT EXISTS (SELECT FROM pg_constraint WHERE conrelid = 'inventory.material_document'::regclass
				AND conname = 'material_document_movement_type_check' AND pg_get_constraintdef(oid) LIKE '%''122''%')`},
		{[]string{"000505_validate_customer_return_stock", "000504_customer_return_stock"}, "sales", "return_receipt_posting", "receipt_id", ""},
	} {
		t.Run(c.names[len(c.names)-1], func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, name := range c.names {
				if err := exec(tx, migrationNamed(t, name).Down); err != nil {
					t.Fatalf("the down of %s: got %v, want it to pass", name, err)
				}
			}
			query, args := `SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2
				AND column_name = $3)`, []any{c.schema, c.table, c.column}
			if c.check != "" {
				query, args = c.check, nil
			}
			var left bool
			if err := tx.QueryRow(ctx, query, args...).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left {
				t.Errorf("%s.%s: got the up's change still there, want the down to revert it", c.table, c.column)
			}
			for i := len(c.names) - 1; i >= 0; i-- {
				if err := exec(tx, migrationNamed(t, c.names[i]).Up); err != nil {
					t.Errorf("the up of %s again: got %v, want it to apply", c.names[i], err)
				}
			}
		})
	}
}
