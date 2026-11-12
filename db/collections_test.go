package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCollectionsPermissions (T76a, migration 000273; T76c, 000270;
// ADR-0065 §7): the dunning permissions and the worklist's are seeded and
// are no approval permissions.
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
	if got, want := strings.Join(codes, " "), "collections.dunning.read collections.dunning.run collections.worklist.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestDunningRunRules (T76a, migration 000273; ADR-0065 §3-§4): a dunning
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
	// An item dunned before was dunned as often as its previous level, at least (T76b, 000269).
	const item = `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
		line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level, previous_dunnings)
		VALUES ($1, $2, $3, $4, 2026, $5, 1, 'DR1', '2026-03-01', '2026-03-31', $6, 'EUR', $7, $8, $9::smallint, $10, $9::smallint)`
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
			return all(tx, step(run, s.tenant, s.run, "cancelled", "EUR", 0, 0, 0))
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

// TestDunningRunConfirmation (T76b, migration 000269; ADR-0065 §8): a
// proposed run moves to confirmed, once, by its status, version and who
// changed it alone; a dunned item is one of the items its run proposed,
// at a level from 1 to 9, recorded once per run and fixed; an item is
// proposed with the times it was dunned before, at least its previous
// level and none exactly when it has none.
func TestDunningRunConfirmation(t *testing.T) {
	need(t)
	const run = `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
		overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 1, 1, 10000, $1, $1)`
	const notice = `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount)
		VALUES ($1, $2, $3, gen_random_uuid(), 'BP1', 1, 1, 10000)`
	const item = `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
		line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level)
		VALUES ($1, $2, $3, 1, 2026, $4, 1, 'DR1', '2026-03-01', '2026-03-31', 91, 'EUR', 10000, 10000, 0, 1)`
	const confirm = `UPDATE collections.dunning_run SET status = 'confirmed', version = version + 1, updated_by = $1 WHERE id = $2`
	const dunned = `INSERT INTO collections.dunned_item (tenant_id, fiscal_year, journal_entry_id, line_no, dunning_run_id, level, dunning_date)
		VALUES ($1, 2026, $2, 1, $3, $4, '2026-06-30')`
	// again is the item in a later run's notice, from a previous level, dunned so many times before.
	const again = `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
		line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level, previous_dunnings)
		VALUES ($1, $2, $3, 1, 2026, $4, 1, 'DR1', '2026-03-01', '2026-03-31', 91, 'EUR', 10000, 10000, $5::smallint, $5::smallint + 1, $6)`
	type scene struct{ tenant, run, notice, entry, run2, notice2 uuid.UUID }
	// proposed is a run with one notice of one item.
	proposed := func(tx pgx.Tx, s scene) error {
		return all(tx, step(run, s.tenant, s.run), step(notice, s.tenant, s.notice, s.run), step(item, s.tenant, s.run, s.notice, s.entry))
	}
	cases := []struct {
		name string
		then func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a run confirmed with its item recorded", func(s scene) []stmt {
			return []stmt{step(confirm, s.tenant, s.run), step(dunned, s.tenant, s.entry, s.run, 1)}
		}, ""},
		{"a run confirmed twice", func(s scene) []stmt {
			return []stmt{step(confirm, s.tenant, s.run), step(confirm, s.tenant, s.run)}
		}, "only a proposed run changes, and only by being confirmed"},
		{"a confirmed run proposed again", func(s scene) []stmt {
			return []stmt{step(confirm, s.tenant, s.run), step(`UPDATE collections.dunning_run SET status = 'proposed' WHERE id = $1`, s.run)}
		}, "only a proposed run changes, and only by being confirmed"},
		{"a proposed run changed without being confirmed", func(s scene) []stmt {
			return []stmt{step(`UPDATE collections.dunning_run SET version = version + 1 WHERE id = $1`, s.run)}
		}, "only a proposed run changes, and only by being confirmed"},
		{"a run's key date changed with its confirmation", func(s scene) []stmt {
			return []stmt{step(`UPDATE collections.dunning_run SET status = 'confirmed', key_date = '2026-07-31' WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"a run's totals changed with its confirmation", func(s scene) []stmt {
			return []stmt{step(`UPDATE collections.dunning_run SET status = 'confirmed', overdue_amount = 1 WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"an item recorded that the run did not propose", func(s scene) []stmt {
			return []stmt{step(confirm, s.tenant, s.run), step(dunned, s.tenant, id(), s.run, 1)}
		}, "dunned_item_proposed_fkey"},
		{"an item recorded for another run than proposed it", func(s scene) []stmt {
			other := id()
			return []stmt{step(run, s.tenant, other), step(dunned, s.tenant, s.entry, other, 1)}
		}, "dunned_item_proposed_fkey"},
		{"an item recorded twice for its run", func(s scene) []stmt {
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 1), step(dunned, s.tenant, s.entry, s.run, 1)}
		}, "dunned_item_pkey"},
		{"the item recorded for a second run that proposed it", func(s scene) []stmt {
			other, its := id(), id()
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 1), step(run, s.tenant, other), step(notice, s.tenant, its, other),
				step(item, s.tenant, other, its, s.entry), step(dunned, s.tenant, s.entry, other, 1)}
		}, ""},
		{"an item proposed as dunned twice, at level 1", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run2), step(notice, s.tenant, s.notice2, s.run2), step(again, s.tenant, s.run2, s.notice2, s.entry, 1, 2)}
		}, ""},
		{"an item proposed as never dunned, at a level", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run2), step(notice, s.tenant, s.notice2, s.run2), step(again, s.tenant, s.run2, s.notice2, s.entry, 1, 0)}
		}, "dunning_notice_item_dunnings_check"},
		{"an item proposed as dunned, at no level", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run2), step(notice, s.tenant, s.notice2, s.run2), step(again, s.tenant, s.run2, s.notice2, s.entry, 0, 1)}
		}, "dunning_notice_item_dunnings_check"},
		{"an item proposed at a higher level than the times it was dunned", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run2), step(notice, s.tenant, s.notice2, s.run2), step(again, s.tenant, s.run2, s.notice2, s.entry, 2, 1)}
		}, "dunning_notice_item_dunnings_check"},
		{"an item recorded at level 0", func(s scene) []stmt {
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 0)}
		}, "dunned_item_level_check"},
		{"an item recorded at level 10", func(s scene) []stmt {
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 10)}
		}, "dunned_item_level_check"},
		{"a recorded item changed", func(s scene) []stmt {
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 1), step(`UPDATE collections.dunned_item SET level = 2`)}
		}, "permission denied"},
		{"a recorded item deleted", func(s scene) []stmt {
			return []stmt{step(dunned, s.tenant, s.entry, s.run, 1), step(`DELETE FROM collections.dunned_item`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), run: id(), notice: id(), entry: id(), run2: id(), notice2: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := proposed(tx, s); err != nil {
					t.Fatalf("the proposal: got %v, want no error", err)
				}
				return all(tx, c.then(s)...)
			})
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

// TestDunningRowsAreFixedForTheOwner (T76a, migration 000273; T76b,
// 000269): a notice, an item and a recorded level are insert-only for
// every role, the tables' owner included: what a run proposed and what a
// confirmation recorded is not rewritten by a later migration's statement
// or a repair either.
func TestDunningRowsAreFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, run, notice, entry := id(), id(), id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
		step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
			overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 1, 1, 100, $1, $1)`, tenant, run),
		step(`INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount)
			VALUES ($1, $2, $3, gen_random_uuid(), 'BP1', 1, 1, 100)`, tenant, notice, run),
		step(`INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
			line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level)
			VALUES ($1, $2, $3, 1, 2026, $4, 1, 'DR1', '2026-03-01', '2026-03-31', 91, 'EUR', 100, 100, 0, 1)`, tenant, run, notice, entry),
		step(`INSERT INTO collections.dunned_item (tenant_id, fiscal_year, journal_entry_id, line_no, dunning_run_id, level, dunning_date)
			VALUES ($1, 2026, $2, 1, $3, 1, '2026-06-30')`, tenant, entry, run)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"dunned_item", "dunning_notice_item", "dunning_notice"} {
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
