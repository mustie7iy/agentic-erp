package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// GR/IR positions migrated from SAP (T72f, migration 000167): the quantity a
// take-over posts is kept apart, and a position is open until invoiced,
// cleared and migrated make up what was received and its balance is zero.
func TestGRIRMigratedOpen(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name                         string
		received, invoiced, migrated string
		amount                       int64
		open                         bool
	}{
		{"received in SAP, not invoiced", "0", "0", "-4", -1000, true},
		{"invoiced after cutover", "0", "4", "-4", 0, false},
		{"received on the platform too", "2", "4", "-2", 0, false},
		{"partly invoiced", "0", "1", "-4", -750, true},
		{"a residual of SAP's", "0", "0", "0", -5, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			po := id()
			var open bool
			mustApp(t, f.tenant, func(tx pgx.Tx) error {
				if err := exec(tx, `INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id, currency_tc,
					currency_lc, currency_gc, uom_code, received, invoiced, migrated, amount_tc, amount_lc, amount_gc, last_posting_date)
					VALUES ($1, $2, $3, 1, $4, 'USD', 'USD', 'EUR', 'EA', $5, $6, $7, $8, $8, $8, '2026-08-20')`,
					f.tenant, f.entity, po, f.expense, c.received, c.invoiced, c.migrated, c.amount); err != nil {
					return err
				}
				return tx.QueryRow(t.Context(), `SELECT is_open FROM ledger.gr_ir_position WHERE purchase_order_id = $1`, po).Scan(&open)
			})
			if open != c.open {
				t.Errorf("is_open: got %t, want %t", open, c.open)
			}
		})
	}
}
