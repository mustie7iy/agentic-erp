package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCollectionsPermissions (T76a, migration 000268; ADR-0065 §7): the
// dunning permissions are seeded and are no approval permissions.
func TestCollectionsPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'collections.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "collections.dunning.read collections.dunning.run"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestDunningRunRules (T76a, migration 000268; ADR-0065 §3-§4): a dunning
// run is stored proposed with its totals; a notice is one customer's in
// its run, at a level from 1 to 9, with at least one item; an item is a
// positive amount more than zero days overdue, proposed once in its run,
// at a level from 1 that is its previous level or one above; what a run
// proposed is fixed and never deleted.
func TestDunningRunRules(t *testing.T) {
	need(t)
	const run = `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
		overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', $3, $4, $5, $6, $7, $1, $1)`
	const notice = `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	const item = `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
		line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level)
		VALUES ($1, $2, $3, $4, 2026, $5, 1, 'DR1', '2026-03-01', '2026-03-31', $6, 'EUR', $7, $8, $9, $10)`
	type scene struct{ tenant, run, notice, customer, entry uuid.UUID }
	proposed := func(s scene) stmt { return step(run, s.tenant, s.run, "proposed", "EUR", 1, 1, 10000) }
	noticed := func(s scene) stmt { return step(notice, s.tenant, s.notice, s.run, s.customer, "BP1", 1, 1, 10000) }
	// itemAt is item 1 of the scene's notice: its days overdue, amounts and levels.
	itemAt := func(s scene, days, amount, local, previous, level int) stmt {
		return step(item, s.tenant, s.run, s.notice, 1, s.entry, days, amount, local, previous, level)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a run with a notice and its item", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1))
		}, ""},
		{"a run without notices", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "proposed", "EUR", 0, 0, 0))
		}, ""},
		{"a run in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "confirmed", "EUR", 0, 0, 0))
		}, "dunning_run_status_check"},
		{"a run in an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "proposed", "XXA", 0, 0, 0))
		}, "dunning_run_currency_fkey"},
		{"a negative count of notices", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "proposed", "EUR", -1, 0, 0))
		}, "dunning_run_notice_count_check"},
		{"fewer items than notices", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "proposed", "EUR", 2, 1, 100))
		}, "dunning_run_item_count_check"},
		{"a negative overdue amount", func(tx pgx.Tx, s scene) error {
			return all(tx, step(run, s.tenant, s.run, "proposed", "EUR", 0, 0, -1))
		}, "dunning_run_overdue_amount_check"},
		{"a run changed", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(`UPDATE collections.dunning_run SET overdue_amount = 1, version = version + 1`))
		}, "permission denied"},
		{"a run deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(`DELETE FROM collections.dunning_run`))
		}, "permission denied"},
		{"a notice of no run", func(tx pgx.Tx, s scene) error {
			return all(tx, noticed(s))
		}, "dunning_notice_tenant_id_dunning_run_id_fkey"},
		{"a customer's second notice in a run", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), step(notice, s.tenant, id(), s.run, s.customer, "BP1", 1, 1, 10000))
		}, "dunning_notice_customer_once"},
		{"the customer's notice in another run", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, proposed(s), noticed(s), step(run, s.tenant, other, "proposed", "EUR", 1, 1, 10000),
				step(notice, s.tenant, id(), other, s.customer, "BP1", 1, 1, 10000))
		}, ""},
		{"a notice without the customer's number", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(notice, s.tenant, s.notice, s.run, s.customer, "", 1, 1, 10000))
		}, "dunning_notice_customer_number_check"},
		{"a notice at level 0", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(notice, s.tenant, s.notice, s.run, s.customer, "BP1", 0, 1, 10000))
		}, "dunning_notice_level_check"},
		{"a notice at level 10", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(notice, s.tenant, s.notice, s.run, s.customer, "BP1", 10, 1, 10000))
		}, "dunning_notice_level_check"},
		{"a notice without items", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(notice, s.tenant, s.notice, s.run, s.customer, "BP1", 1, 0, 0))
		}, "dunning_notice_item_count_check"},
		{"a notice's negative overdue amount", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), step(notice, s.tenant, s.notice, s.run, s.customer, "BP1", 1, 1, -1))
		}, "dunning_notice_overdue_amount_check"},
		{"a notice changed", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), step(`UPDATE collections.dunning_notice SET level = 2`))
		}, "permission denied"},
		{"a notice deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), step(`DELETE FROM collections.dunning_notice`))
		}, "permission denied"},
		{"an item of no notice", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), itemAt(s, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_notice_fkey"},
		{"an item naming another run than its notice's", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, proposed(s), noticed(s), step(run, s.tenant, other, "proposed", "EUR", 0, 0, 0),
				step(item, s.tenant, other, s.notice, 1, s.entry, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_notice_fkey"},
		{"an item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), step(item, s.tenant, s.run, s.notice, 0, s.entry, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_item_no_check"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1), step(item, s.tenant, s.run, s.notice, 1, id(), 91, 100, 100, 0, 1))
		}, "dunning_notice_item_pkey"},
		{"an open item proposed twice in a run", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1), step(item, s.tenant, s.run, s.notice, 2, s.entry, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_once_per_run"},
		{"the open item proposed in another run", func(tx pgx.Tx, s scene) error {
			other, its := id(), id()
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1), step(run, s.tenant, other, "proposed", "EUR", 1, 1, 10000),
				step(notice, s.tenant, its, other, s.customer, "BP1", 1, 1, 10000), step(item, s.tenant, other, its, 1, s.entry, 92, 10000, 10000, 0, 1))
		}, ""},
		{"an item not overdue", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 0, 10000, 10000, 0, 1))
		}, "dunning_notice_item_overdue"},
		{"a credit item", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, -10000, 10000, 0, 1))
		}, "dunning_notice_item_amount_positive"},
		{"an item of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 0, 0, 0, 1))
		}, "dunning_notice_item_amount_positive"},
		{"an item booked at zero in the company's currency", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 1, 0, 0, 1))
		}, ""},
		{"an item booked below zero in the company's currency", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 1, -1, 0, 1))
		}, "dunning_notice_item_amount_lc_check"},
		{"an item at level 0", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 0))
		}, "dunning_notice_item_level_check"},
		{"an item at level 10", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 9, 10))
		}, "dunning_notice_item_level_check"},
		{"an item last dunned at a negative level", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, -1, 1))
		}, "dunning_notice_item_one_level"},
		{"an item rising two levels", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 2))
		}, "dunning_notice_item_one_level"},
		{"an item falling a level", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 2, 1))
		}, "dunning_notice_item_one_level"},
		{"an item rising one level", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 1, 2))
		}, ""},
		{"an item staying at its level", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 3, 3))
		}, ""},
		{"an item in an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s),
				step(strings.Replace(item, "'EUR'", "'XXA'", 1), s.tenant, s.run, s.notice, 1, s.entry, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_currency_fkey"},
		{"an item without a document number", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s),
				step(strings.Replace(item, "'DR1'", "''", 1), s.tenant, s.run, s.notice, 1, s.entry, 91, 10000, 10000, 0, 1))
		}, "dunning_notice_item_document_number_check"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1), step(`UPDATE collections.dunning_notice_item SET level = 1`))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, proposed(s), noticed(s), itemAt(s, 91, 10000, 10000, 0, 1), step(`DELETE FROM collections.dunning_notice_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), run: id(), notice: id(), customer: id(), entry: id()}
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

// TestDunningRowsAreFixedForTheOwner (T76a, migration 000268): a notice
// and an item are insert-only for every role, the tables' owner included:
// what a run proposed is not rewritten by a later migration's statement
// or a repair either.
func TestDunningRowsAreFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, run, notice := id(), id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
		step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
			overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 1, 1, 100, $1, $1)`, tenant, run),
		step(`INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount)
			VALUES ($1, $2, $3, gen_random_uuid(), 'BP1', 1, 1, 100)`, tenant, notice, run),
		step(`INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
			line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level)
			VALUES ($1, $2, $3, 1, 2026, gen_random_uuid(), 1, 'DR1', '2026-03-01', '2026-03-31', 91, 'EUR', 100, 100, 0, 1)`, tenant, run, notice)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"dunning_notice_item", "dunning_notice"} {
		for _, change := range []string{"UPDATE collections." + table + " SET level = 1 WHERE tenant_id = $1", "DELETE FROM collections." + table + " WHERE tenant_id = $1"} {
			t.Run(change, func(t *testing.T) {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					t.Fatal(err)
				}
				_, err := tx.Exec(ctx, change, tenant)
				failsWith(t, err, "insert-only")
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
