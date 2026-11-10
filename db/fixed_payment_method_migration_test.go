package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestFixedPaymentMethodKeepsPaymentsWrittenBefore: the payments a release
// before 000249 wrote mean what they meant once items may fix a payment
// method (ADR-0038; 000249-000253): none fixes its method, and a run
// still holds one items payment per supplier for them, now by
// payment_items_once_unfixed, the index payment_items_once gave way to.
func TestFixedPaymentMethodKeepsPaymentsWrittenBefore(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000249_fixed_payment_method")
	f := fixtureIDs()
	run := id()
	pay := func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by, kind)
			VALUES ($1, $2, $3, $4, $5, 100, 'USD', left(md5(random()::text), 20), $6, 'items')`, f.tenant, id(), run, f.supplier, id(), f.actor)
	}
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, run_date, due_by, currency, total_amount, status, proposed_by, created_by, updated_by)
			VALUES ($1,$2,'PR1',$3,'2026-07-31','2026-07-31','USD',100,'proposed',$4,$4,$4)`, f.tenant, run, f.entity, f.actor); err != nil {
			return err
		}
		return pay(tx)
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var fixed, old bool
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(context.Background(), `SELECT bool_or(method_fixed) FROM payments.payment`).Scan(&fixed); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT to_regclass('payments.payment_items_once') IS NOT NULL`).Scan(&old)
	})
	if fixed {
		t.Errorf("a payment written before, method_fixed: got true, want false")
	}
	if old {
		t.Errorf("payment_items_once after 000253: got it still there, want it dropped")
	}
	err := pgx.BeginFunc(context.Background(), conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", f.tenant.String()); err != nil {
			return err
		}
		return pay(tx)
	})
	failsOn(t, err, "payment_items_once_unfixed")
}
