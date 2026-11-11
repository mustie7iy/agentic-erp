package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GR/IR clearings (T72e, migration 000165): a clearing starts pending and
// is decided once, posted naming its entry; what it clears never changes;
// one is pending per order; its items never change (no grant, and
// insert-only) and never clear nothing; an item names the account it
// clears against whole or not at all (000264); the ledger's record of clearing
// entries never changes either; and a position is
// open until its invoiced and cleared quantities make up what was received.

// clearing inserts a closed order of f and a pending clearing of it with
// one item, and returns the order and the clearing.
func (f fixture) clearing(t *testing.T) (po, clearing uuid.UUID) {
	t.Helper()
	price := int64(250)
	clearing = id()
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if po, err = f.order(tx, &price); err != nil {
			return err
		}
		if err := insertClearing(tx, f, clearing, po, "pending"); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO procurement.gr_ir_clearing_item (tenant_id, gr_ir_clearing_id, po_item_no, quantity, uom_code, amount)
			VALUES ($1, $2, 1, -4, 'EA', -1000)`, f.tenant, clearing)
	})
	return po, clearing
}

func insertClearing(tx pgx.Tx, f fixture, clearing, po uuid.UUID, status string) error {
	return exec(tx, `INSERT INTO procurement.gr_ir_clearing (tenant_id, id, purchase_order_id, legal_entity_id, status, reason, posting_date, currency,
		offset_account_id, offset_account_number, offset_account_name, approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'Short delivery', '2026-08-20', 'USD', $6, '590000', 'Price differences', gen_random_uuid(), $7, $7)`,
		f.tenant, clearing, po, f.entity, status, f.expense, f.actor)
}

func TestGRIRClearingRules(t *testing.T) {
	need(t)
	update := func(clearing uuid.UUID, set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.gr_ir_clearing SET `+set+`, version = version + 1 WHERE id = $1`, clearing)
		}
	}
	const posted = `status = 'posted', journal_entry_id = gen_random_uuid(), journal_entry_year = 2026, journal_entry_number = '5000000001'`
	for _, c := range []struct{ name, set, want string }{
		{"posted naming its entry", posted, ""},
		{"rejected with a comment", "status = 'rejected', decision_comment = 'No'", ""},
		{"cancelled", "status = 'cancelled'", ""},
		{"posted without its entry", "status = 'posted'", "gr_ir_clearing_posted"},
		{"an entry while pending", "journal_entry_id = gen_random_uuid(), journal_entry_year = 2026, journal_entry_number = '1'", "cannot go from pending to pending"},
		{"its reason changed", "status = 'rejected', reason = 'Other'", "changes only its status"},
		{"its posting date changed", "status = 'rejected', posting_date = '2026-08-21'", "changes only its status"},
		{"its account changed", "status = 'rejected', offset_account_id = gen_random_uuid()", "changes only its status"},
		{"its request changed", "status = 'rejected', approval_request_id = gen_random_uuid()", "changes only its status"},
		{"its proposer changed", "status = 'rejected', created_by = gen_random_uuid()", "changes only its status"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, clearing := f.clearing(t)
			err := asApp(t, f.tenant, update(clearing, c.set))
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("decided twice", func(t *testing.T) {
		f := newFixture(t)
		_, clearing := f.clearing(t)
		mustApp(t, f.tenant, update(clearing, "status = 'rejected'"))
		failsWith(t, asApp(t, f.tenant, update(clearing, "status = 'cancelled'")), "cannot go from rejected to cancelled")
	})
	t.Run("inserted decided", func(t *testing.T) {
		f := newFixture(t)
		po, _ := f.clearing(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return insertClearing(tx, f, id(), po, "posted") }), "starts pending")
	})
	t.Run("a second pending clearing of the order", func(t *testing.T) {
		f := newFixture(t)
		po, clearing := f.clearing(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return insertClearing(tx, f, id(), po, "pending") }), "gr_ir_clearing_one_pending")
		mustApp(t, f.tenant, update(clearing, "status = 'cancelled'"))
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return insertClearing(tx, f, id(), po, "pending") })
	})
	t.Run("an item clearing nothing", func(t *testing.T) {
		f := newFixture(t)
		_, clearing := f.clearing(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.gr_ir_clearing_item (tenant_id, gr_ir_clearing_id, po_item_no, quantity, uom_code, amount)
				VALUES ($1, $2, 2, 0, 'EA', 0)`, f.tenant, clearing)
		}), "gr_ir_clearing_item_open")
	})
	t.Run("an item's account", func(t *testing.T) {
		const insert = `INSERT INTO procurement.gr_ir_clearing_item (tenant_id, gr_ir_clearing_id, po_item_no, quantity, uom_code, amount,
			offset_account_id, offset_account_number, offset_account_name) VALUES ($1, $2, $3, -1, 'EA', -250, $4, $5, $6)`
		f := newFixture(t)
		_, clearing := f.clearing(t)
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, insert, f.tenant, clearing, 2, f.expense, "169000", "Asset clearing")
		})
		for name, account := range map[string][3]any{
			"without its number": {f.expense, nil, "Asset clearing"},
			"without its name":   {f.expense, "169000", nil},
			"without its id":     {nil, "169000", "Asset clearing"},
		} {
			t.Run(name, func(t *testing.T) {
				failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
					return exec(tx, insert, f.tenant, clearing, 3, account[0], account[1], account[2])
				}), "gr_ir_clearing_item_account")
			})
		}
	})
	t.Run("an item changed", func(t *testing.T) {
		f := newFixture(t)
		_, clearing := f.clearing(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.gr_ir_clearing_item SET amount = 1 WHERE gr_ir_clearing_id = $1`, clearing)
		}), "permission denied")
	})
}

func TestGRIRClearingEntries(t *testing.T) {
	need(t)
	f := newFixture(t)
	var entry uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if entry, err = f.entry(tx, 8, "2026-08-20", line{account: f.expense, amount: 100}, line{account: f.bank, amount: -100}); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.gr_ir_clearing_entry (tenant_id, fiscal_year, journal_entry_id) VALUES ($1, 2026, $2)`, f.tenant, entry)
	})
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.gr_ir_clearing_entry SET fiscal_year = 2026 WHERE journal_entry_id = $1`, entry)
	}), "permission denied")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.gr_ir_clearing_entry (tenant_id, fiscal_year, journal_entry_id) VALUES ($1, 2026, gen_random_uuid())`, f.tenant)
	}), "violates foreign key constraint")
	// A position is open until invoiced plus cleared is what was received
	// and its balance is zero.
	for _, c := range []struct {
		name                       string
		received, invoiced, clears string
		amount                     int64
		open                       bool
	}{
		{"received, not invoiced", "4", "0", "0", -1000, true},
		{"cleared", "4", "0", "4", 0, false},
		{"partly cleared", "4", "1", "2", 0, true},
		{"invoiced more, cleared back", "2", "3", "-1", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			po := id()
			var open bool
			mustApp(t, f.tenant, func(tx pgx.Tx) error {
				if err := exec(tx, `INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id, currency_tc,
					currency_lc, currency_gc, uom_code, received, invoiced, cleared, amount_tc, amount_lc, amount_gc, last_posting_date)
					VALUES ($1, $2, $3, 1, $4, 'USD', 'USD', 'EUR', 'EA', $5, $6, $7, $8, $8, $8, '2026-08-20')`,
					f.tenant, f.entity, po, f.expense, c.received, c.invoiced, c.clears, c.amount); err != nil {
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
