package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestAccountDeterminationRemovalRules (migration 000464; ADR-0048 As
// built batch 93): the application deletes no account determination row
// itself; recording a key's removal deletes the row naming that very
// account, as the owner, and only the tenant's own; a removal of a key
// that is not determined, or names another account, and one of a key that
// stays once activated (gr_ir, down_payment, retained_earnings) are
// refused; a removal is never changed or deleted, by the owner either; a
// removed key may be determined again and removed again.
func TestAccountDeterminationRemovalRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	other := newFixture(t)
	determine := func(key string) stmt {
		return step(`INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, $3, $4, $5)`,
			f.tenant, f.entity, key, f.expense, f.actor)
	}
	remove := func(key string) stmt {
		return step(`INSERT INTO ledger.account_determination_removal (tenant_id, legal_entity_id, key, account_id, removed_by) VALUES ($1, $2, $3, $4, $5)`,
			f.tenant, f.entity, key, f.expense, f.actor)
	}
	mustApp(t, other.tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_loss', $3, $4)`,
			other.tenant, other.entity, other.expense, other.actor))
	})
	for _, k := range []struct {
		name  string
		stmts []stmt
		want  string
	}{
		{"a row deleted by the application", []stmt{determine("fx_loss"), step(`DELETE FROM ledger.account_determination WHERE key = 'fx_loss'`)}, "permission denied"},
		{"a removal of a key not determined", []stmt{remove("fx_loss")}, "does not name account"},
		{"a removal naming another account", []stmt{determine("fx_loss"), step(`INSERT INTO ledger.account_determination_removal
			(tenant_id, legal_entity_id, key, account_id, removed_by) VALUES ($1, $2, 'fx_loss', $3, $4)`, f.tenant, f.entity, f.bank, f.actor)},
			"does not name account"},
		{"a removal of another tenant's key", []stmt{step(`INSERT INTO ledger.account_determination_removal
			(tenant_id, legal_entity_id, key, account_id, removed_by) VALUES ($1, $2, 'fx_loss', $3, $4)`, other.tenant, other.entity, other.expense, f.actor)},
			"row-level security"},
		{"a removal of gr_ir", []stmt{determine("gr_ir"), remove("gr_ir")}, "account_determination_removal_kept"},
		{"a removal of down_payment", []stmt{determine("down_payment"), remove("down_payment")}, "account_determination_removal_kept"},
		{"a removal of retained_earnings", []stmt{determine("retained_earnings"), remove("retained_earnings")}, "account_determination_removal_kept"},
		{"a removal changed", []stmt{determine("fx_loss"), remove("fx_loss"), step(`UPDATE ledger.account_determination_removal SET removed_by = $1`, f.tenant)},
			"permission denied"},
		{"a removal deleted", []stmt{determine("fx_loss"), remove("fx_loss"), step(`DELETE FROM ledger.account_determination_removal`)}, "permission denied"},
	} {
		t.Run(k.name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, k.stmts...) }), k.want)
		})
	}
	t.Run("removed, determined again and removed again", func(t *testing.T) {
		count := func(tx pgx.Tx, table string) (n int) {
			t.Helper()
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM `+pgx.Identifier{"ledger", table}.Sanitize()+` WHERE key = 'fx_loss'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		var left, removals int
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := all(tx, determine("fx_loss"), remove("fx_loss")); err != nil {
				return err
			}
			if got := count(tx, "account_determination"); got != 0 {
				t.Errorf("after the removal: got %d rows, want none", got)
			}
			// Removed again in a transaction of its own: a removal is keyed
			// by its transaction's time (removed_at).
			return all(tx, determine("fx_loss"))
		})
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := all(tx, remove("fx_loss")); err != nil {
				return err
			}
			left, removals = count(tx, "account_determination"), count(tx, "account_determination_removal")
			return nil
		})
		if left != 0 || removals != 2 {
			t.Errorf("got %d rows left and %d removals, want none left and 2 removals", left, removals)
		}
		mustApp(t, other.tenant, func(tx pgx.Tx) error {
			if got := count(tx, "account_determination"); got != 1 {
				t.Errorf("the other tenant's key: got %d rows, want it kept", got)
			}
			return nil
		})
	})
	t.Run("as the owner", func(t *testing.T) {
		for name, sql := range map[string]string{
			"changed": `UPDATE ledger.account_determination_removal SET removed_by = removed_by`,
			"deleted": `DELETE FROM ledger.account_determination_removal`,
		} {
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				tx, err := testDB.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				err = all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, f.tenant.String()), step(sql))
				failsWith(t, err, "ledger.account_determination_removal is insert-only")
			})
		}
	})
}

// TestAccountDeterminationRemovalDown (migration 000464): the revert is
// refused while a removal is recorded, whose record it would lose, and
// with none drops the table and its trigger.
func TestAccountDeterminationRemovalDown(t *testing.T) {
	need(t)
	f := newFixture(t)
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
			f.tenant, f.entity, f.expense, f.actor),
			step(`INSERT INTO ledger.account_determination_removal (tenant_id, legal_entity_id, key, account_id, removed_by) VALUES ($1, $2, 'fx_gain', $3, $4)`,
				f.tenant, f.entity, f.expense, f.actor))
	})
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	down := migrationNamed(t, "000464_account_determination_removal").Down
	if err := savepoint(tx, down); err == nil {
		t.Fatal("000464's down with a removal recorded: got it reverted, want it refused")
	} else {
		failsWith(t, err, "reverting would lose the record of their removal")
	}
	// Every tenant's removals go, in this transaction only: past the
	// insert-only trigger, as a restore would load none.
	if err := all(tx, step(`SET LOCAL session_replication_role = replica`), step(`DELETE FROM ledger.account_determination_removal`),
		step(`SET LOCAL session_replication_role = origin`)); err != nil {
		t.Fatal(err)
	}
	if err := exec(tx, down); err != nil {
		t.Fatalf("000464's down with no removal: %v", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('ledger.account_determination_removal') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("after 000464's down: got the removal table, want it dropped")
	}
}
