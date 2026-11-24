package db

import (
	"context"
	"testing"
)

// TestDiscountAccountKeys (migrations 000460 and 000461; ADR-0063 and
// ADR-0061 As built batch 92): after the last migration the account
// determination takes cash_discount_granted and supplier_rebate, its key
// check validated, and still refuses a key it does not know. Reverting
// 000461 leaves the check unvalidated; 000460's down then refuses while a
// company names an account for either key, and with none restores
// 000411's check, validated, both keys refused.
func TestDiscountAccountKeys(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Foreign keys aside: the check is what is tested.
	if err := exec(tx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	tenant := id()
	insert := func(key string) error { return savepoint(tx, movementTypeKey, tenant, key) }
	validated := func(stage string, want bool) {
		t.Helper()
		var got bool
		if err := tx.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'account_determination_key_check'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s, account_determination_key_check validated: got %v, want %v", stage, got, want)
		}
	}
	for _, key := range []string{"cash_discount_granted", "supplier_rebate"} {
		if err := insert(key); err != nil {
			t.Errorf("after the last migration, %s: got %v, want it taken", key, err)
		}
	}
	if err := insert("cash_discount_taken"); !violates(err, "account_determination_key_check") {
		t.Errorf("after the last migration, an unknown key: got %v, want a violation of account_determination_key_check", err)
	}
	validated("after the last migration", true)
	if err := exec(tx, migrationNamed(t, "000461_validate_account_determination_discount_keys").Down); err != nil {
		t.Fatalf("000461's down: %v", err)
	}
	validated("after reverting 000461", false)
	down := migrationNamed(t, "000460_account_determination_discount_keys").Down
	if err := savepoint(tx, down); err == nil {
		t.Fatal("000460's down with the keys set: got it reverted, want it refused")
	} else {
		failsWith(t, err, "the previous version cannot hold them")
	}
	if err := exec(tx, `DELETE FROM ledger.account_determination WHERE key IN ('cash_discount_granted', 'supplier_rebate')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(tx, down); err != nil {
		t.Fatalf("000460's down with neither key set: %v", err)
	}
	validated("after reverting 000460", true)
	for _, key := range []string{"cash_discount_granted", "supplier_rebate"} {
		if err := insert(key); !violates(err, "account_determination_key_check") {
			t.Errorf("after reverting 000460, %s: got %v, want a violation of account_determination_key_check", key, err)
		}
	}
	if err := insert("payment_difference_expense"); err != nil {
		t.Errorf("after reverting 000460, payment_difference_expense: got %v, want it taken", err)
	}
}
