package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// clearedPair posts an invoice and its payment of the fixture's supplier
// in June and clears them with clearing c, the payment being the clearing
// entry: what TestReversalAndClearingHappenOnce clears.
func (f fixture) clearedPair(t *testing.T, c uuid.UUID) (inv, pay uuid.UUID) {
	t.Helper()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		var err error
		if inv, err = f.entry(tx, 6, "2026-06-01", line{account: f.expense, amount: 700}, line{account: f.ap, amount: -700, partner: &f.supplier}); err != nil {
			return err
		}
		if pay, err = f.entry(tx, 6, "2026-06-20", line{account: f.ap, amount: 700, partner: &f.supplier}, line{account: f.bank, amount: -700}); err != nil {
			return err
		}
		return all(tx, step(`INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
				VALUES ($1, $2, $3, '2026-06-20', 2026, $4, $5)`, f.tenant, c, f.entity, pay, f.actor),
			step(`INSERT INTO ledger.clearing_item VALUES ($1, 2026, $2, 2, $3), ($1, 2026, $4, 1, $3)`, f.tenant, inv, c, pay))
	})
	return inv, pay
}

// TestClearingResetRules (T75i, migration 000364; ADR-0063 §6 As built): a
// clearing item is never changed, and deleted only once its clearing is
// reset, the trigger keeping it in clearing_reset_item, or with its tenant
// erased; a clearing is reset once; a reset and its items are never
// changed or deleted; once reset, the lines may be cleared again.
func TestClearingResetRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	c := id()
	inv, pay := f.clearedPair(t, c)
	reset := step(`INSERT INTO ledger.clearing_reset (tenant_id, clearing_id, reset_by) VALUES ($1, $2, $3)`, f.tenant, c, f.actor)
	deleted := step(`DELETE FROM ledger.clearing_item WHERE clearing_id = $1`, c)
	for _, k := range []struct {
		name  string
		stmts []stmt
		want  string
	}{
		{"an item deleted while its clearing holds", []stmt{deleted}, "allowed only of a clearing that is reset"},
		{"an item changed", []stmt{step(`UPDATE ledger.clearing_item SET line_no = 3 WHERE clearing_id = $1`, c)}, "permission denied"},
		{"a reset of no clearing", []stmt{step(`INSERT INTO ledger.clearing_reset (tenant_id, clearing_id, reset_by) VALUES ($1, $2, $3)`, f.tenant, id(), f.actor)},
			"clearing_reset_tenant_id_clearing_id_fkey"},
		{"a clearing reset twice", []stmt{reset, reset}, "clearing_reset_pkey"},
		{"a reset changed", []stmt{reset, step(`UPDATE ledger.clearing_reset SET reset_by = $1`, id())}, "permission denied"},
		{"a reset deleted", []stmt{reset, step(`DELETE FROM ledger.clearing_reset WHERE clearing_id = $1`, c)}, "permission denied"},
		{"a reset item of no reset", []stmt{step(`INSERT INTO ledger.clearing_reset_item VALUES ($1, $2, 2026, $3, 2)`, f.tenant, id(), inv)},
			"clearing_reset_item_tenant_id_clearing_id_fkey"},
		{"a reset item changed", []stmt{reset, deleted, step(`UPDATE ledger.clearing_reset_item SET line_no = 3`)}, "permission denied"},
		{"a reset item deleted", []stmt{reset, deleted, step(`DELETE FROM ledger.clearing_reset_item`)}, "permission denied"},
	} {
		t.Run(k.name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, k.stmts...) }), k.want)
		})
	}
	t.Run("reset, kept and cleared again", func(t *testing.T) {
		var kept, open int
		again := id()
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			ctx := context.Background()
			if err := all(tx, reset, deleted); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM ledger.clearing_reset_item WHERE clearing_id = $1),
					(SELECT count(*) FROM ledger.open_item WHERE partner_id = $2)`, c, f.supplier).Scan(&kept, &open); err != nil {
				return err
			}
			return all(tx, step(`INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
					VALUES ($1, $2, $3, '2026-06-21', 2026, $4, $5)`, f.tenant, again, f.entity, pay, f.actor),
				step(`INSERT INTO ledger.clearing_item VALUES ($1, 2026, $2, 2, $3), ($1, 2026, $4, 1, $3)`, f.tenant, inv, again, pay))
		})
		if err != nil || kept != 2 || open != 2 {
			t.Errorf("got %v, %d lines kept and %d open; want the reset, both lines kept and open, then cleared again", err, kept, open)
		}
	})
	t.Run("as the owner", func(t *testing.T) {
		held := id()
		f.clearedPair(t, held)
		ownerRules(t, f, held)
	})
}

// ownerRules: past the grants, as the owner, the triggers hold, but for
// the erasure of the clearing's tenant (app.erasing).
func ownerRules(t *testing.T, f fixture, c uuid.UUID) {
	t.Helper()
	for _, k := range []struct {
		name, sql, erasing, want string
	}{
		{"an item changed", `UPDATE ledger.clearing_item SET line_no = 3 WHERE clearing_id = $1`, "", "ledger.clearing_item is insert-only"},
		{"an item deleted", `DELETE FROM ledger.clearing_item WHERE clearing_id = $1`, "", "allowed only of a clearing that is reset"},
		{"an item deleted with its tenant erased", `DELETE FROM ledger.clearing_item WHERE clearing_id = $1`, f.tenant.String(), ""},
		{"an item deleted while another tenant is erased", `DELETE FROM ledger.clearing_item WHERE clearing_id = $1`, id().String(),
			"allowed only of a clearing that is reset"},
	} {
		t.Run(k.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			err = all(tx, step(`SELECT set_config('app.tenant_id', $1, true), set_config('app.erasing', $2, true)`, f.tenant.String(), k.erasing), step(k.sql, c))
			if k.want != "" {
				failsWith(t, err, k.want)
				return
			}
			var kept int
			if err == nil {
				err = tx.QueryRow(ctx, `SELECT count(*) FROM ledger.clearing_reset_item WHERE clearing_id = $1`, c).Scan(&kept)
			}
			if err != nil || kept != 0 {
				t.Errorf("got %v with %d lines kept, want the delete and nothing kept", err, kept)
			}
		})
	}
}

// TestIncomingPaymentResetRules (T75i, migration 000364): an undoing is of
// an existing payment and, but for the payment's own (application NULL),
// of one of its applications; each once; with a reason of 1 to 200
// characters and a reversal of its own; never changed or deleted. A
// receipt, an item and a credit may be applied again after it.
func TestIncomingPaymentResetRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, on_account_amount, on_account_line_no, on_account_account_id,
		clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 2026, $5, gen_random_uuid(), '2026-03-21', 'EUR', 10000, 10000, 2, gen_random_uuid(), 2026, gen_random_uuid(), $1)`
	const app = `INSERT INTO receivables.incoming_payment_application (tenant_id, incoming_payment_id, application_no, posting_date, credit_entry_year,
		credit_entry_id, credit_line_no, amount, on_account_amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, '2026-03-22', 2026, $4, 2, 10000, 0, 2026, gen_random_uuid(), $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount)
		VALUES ($1, $2, 1, 2026, $3, 1, 10000)`
	const reset = `INSERT INTO receivables.incoming_payment_reset (tenant_id, incoming_payment_id, application_no, reason, reversal_entry_year,
		reversal_entry_id, created_by) VALUES ($1, $2, $3, $4, 2026, $5, $1)`
	type scene struct{ tenant, company, doc, receipt, credit, invoice uuid.UUID }
	noApp := (*int32)(nil)
	one := int32(1)
	paid := func(s scene) stmt { return step(header, s.tenant, s.doc, "IP1", s.company, s.receipt) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string
	}{
		{"a payment and its application undone, then applied again", func(tx pgx.Tx, s scene) error {
			again := id()
			return all(tx, paid(s), step(item, s.tenant, s.doc, s.invoice), step(app, s.tenant, s.doc, 1, s.credit), step(reset, s.tenant, s.doc, &one, "Wrong", id()),
				step(app, s.tenant, s.doc, 2, s.credit), step(reset, s.tenant, s.doc, noApp, "Wrong", id()),
				step(header, s.tenant, again, "IP2", s.company, s.receipt), step(item, s.tenant, again, s.invoice))
		}, ""},
		{"the payment undone twice", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, noApp, "Wrong", id()), step(reset, s.tenant, s.doc, noApp, "Wrong", id()))
		}, "incoming_payment_reset_once"},
		{"an application undone twice", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit), step(reset, s.tenant, s.doc, &one, "Wrong", id()),
				step(reset, s.tenant, s.doc, &one, "Wrong", id()))
		}, "incoming_payment_reset_once"},
		{"an application the payment has not", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, &one, "Wrong", id()))
		}, "incoming_payment_reset_application_fkey"},
		{"no payment", func(tx pgx.Tx, s scene) error {
			return all(tx, step(reset, s.tenant, s.doc, noApp, "Wrong", id()))
		}, "incoming_payment_reset_payment_fkey"},
		{"two undoings with one reversal", func(tx pgx.Tx, s scene) error {
			rev := id()
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit), step(reset, s.tenant, s.doc, &one, "Wrong", rev),
				step(reset, s.tenant, s.doc, noApp, "Wrong", rev))
		}, "incoming_payment_reset_entry_once"},
		{"an empty reason", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, noApp, "", id()))
		}, "incoming_payment_reset_reason_length"},
		{"a reason of 201 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, noApp, strings.Repeat("x", 201), id()))
		}, "incoming_payment_reset_reason_length"},
		{"an undoing changed", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, noApp, "Wrong", id()), step(`UPDATE receivables.incoming_payment_reset SET reason = 'x'`))
		}, "permission denied"},
		{"an undoing deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(reset, s.tenant, s.doc, noApp, "Wrong", id()), step(`DELETE FROM receivables.incoming_payment_reset`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), doc: id(), receipt: id(), credit: id(), invoice: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
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

// TestClearingResetRevert (T75i, migration 000364's down): the revert is
// refused while a clearing or a payment was reset.
func TestClearingResetRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000364_clearing_reset").Down
	f := newFixture(t)
	c := id()
	f.clearedPair(t, c)
	for name, insert := range map[string]stmt{
		"a clearing reset": step(`INSERT INTO ledger.clearing_reset (tenant_id, clearing_id, reset_by) VALUES ($1, $2, $3)`, f.tenant, c, f.actor),
		"a payment reset": step(`WITH p AS (INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
				receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
				VALUES ($1, $2, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 6000, 2026, gen_random_uuid(), $1) RETURNING id)
			INSERT INTO receivables.incoming_payment_reset (tenant_id, incoming_payment_id, reason, reversal_entry_year, reversal_entry_id, created_by)
			SELECT $1, id, 'Wrong', 2026, gen_random_uuid(), $1 FROM p`, f.tenant, id()),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, f.tenant.String()), insert); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "reverting would lose their history")
		})
	}
}
