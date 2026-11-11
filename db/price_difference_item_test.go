package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestPriceDifferenceItems: what the database enforces of a price
// difference line's recorded order item and tax (000263; B10,
// po:2026-11-10): the line exists and is no GR/IR line, the tax has one
// sign in both currencies, and a row is never changed or removed.
func TestPriceDifferenceItems(t *testing.T) {
	need(t)
	f := newFixture(t)
	order := id()
	const insert = `INSERT INTO ledger.price_difference_item (tenant_id, fiscal_year, journal_entry_id, line_no, purchase_order_id, po_item_no,
		tax_borne_tc, tax_borne_lc) VALUES ($1, 2026, $2, $3, $4, 10, $5, $6)`
	var entry uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if entry, err = f.entry(tx, 8, "2026-08-20", line{account: f.expense, amount: 100}, line{account: f.bank, amount: -100}); err != nil {
			return err
		}
		// Lines 3 and 4: a GR/IR line naming its order item, and its counter line.
		for no, l := range []struct {
			amount int64
			order  *uuid.UUID
		}{{50, &order}, {-50, nil}} {
			var item *int
			var quantity, unit *string
			if l.order != nil {
				ten, one, each := 10, "1", "EA"
				item, quantity, unit = &ten, &one, &each
			}
			if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc, purchase_order_id, po_item_no, quantity, uom_code)
				VALUES ($1, 2026, $2, $3, $4, $5, 8, '2026-08-20', $6, $7, $7, $7, $8, $9, $10::numeric, $11)`,
				f.tenant, entry, no+3, f.entity, f.ledger, f.expense, l.amount, l.order, item, quantity, unit); err != nil {
				return err
			}
		}
		return exec(tx, insert, f.tenant, entry, 1, order, 30, 27)
	})
	for _, c := range []struct {
		name   string
		lineNo int
		tc, lc int64
		want   string
	}{
		{"a line that does not exist", 9, 30, 27, "violates foreign key constraint"},
		{"a line recorded twice", 1, 30, 27, "price_difference_item_pkey"},
		{"a GR/IR line", 3, 30, 27, "is a GR/IR line, not a price difference line"},
		{"tax of opposite signs", 2, 30, -27, "price_difference_item_tax_sign"},
	} {
		t.Run(c.name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, insert, f.tenant, entry, c.lineNo, order, c.tc, c.lc) }), c.want)
		})
	}
	t.Run("a credit's tax, and none", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, insert, f.tenant, entry, 2, order, -30, -27); err != nil {
				return err
			}
			return exec(tx, insert, f.tenant, entry, 4, order, 0, 0)
		})
	})
	for name, change := range map[string]string{
		"changed": `UPDATE ledger.price_difference_item SET tax_borne_tc = 31 WHERE journal_entry_id = $1`,
		"removed": `DELETE FROM ledger.price_difference_item WHERE journal_entry_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, change, entry) }), "permission denied")
		})
	}
	t.Run("another tenant reads none", func(t *testing.T) {
		other := newFixture(t)
		var n int
		mustApp(t, other.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM ledger.price_difference_item`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("rows another tenant reads: got %d, want 0", n)
		}
	})
}
