package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCollectionsPermissions (T76a, migration 000273; T76c, 000278; T86a,
// 000324; T76e, 000354; T76f, 000365; T86d, 000396; ADR-0065 §7): the
// dunning permissions, the worklist's (reading it and recording its work),
// the dispute cases' (resolving them among them) and the dunning blocks'
// are seeded and are no approval permissions.
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
	if got, want := strings.Join(codes, " "), "collections.dispute.read collections.dispute.resolve collections.dispute.write collections.dunning.block collections.dunning.read collections.dunning.run collections.worklist.read collections.worklist.write"; got != want {
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
	// An item dunned before was dunned as often as its previous level, at least (T76b, 000277).
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

// TestDunningRunConfirmation (T76b, migration 000277; ADR-0065 §8): a
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
		{"a run's disputed items changed with its confirmation (T86b, 000325)", func(s scene) []stmt {
			return []stmt{step(`UPDATE collections.dunning_run SET status = 'confirmed', disputed_items = '[{}]' WHERE id = $1`, s.run)}
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
// 000277): a notice, an item and a recorded level are insert-only for
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

// disputeCase inserts an open dispute case of the tenant on the entry's
// line 1; the columns and values after it replace the defaults.
const disputeCase = `INSERT INTO collections.dispute_case (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year, journal_entry_id,
	line_no, document_number, currency, disputed_amount, currency_lc, disputed_amount_lc, reason, status, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 2026, $3, 1, 'DR1', 'EUR', 100, 'EUR', 100, 'Price', 'open', $1, $1)`

// disputeCaseItems gives each case of the tenant that has none its one
// item, its own line, open while the case is.
const disputeCaseItems = `INSERT INTO collections.dispute_case_item (tenant_id, dispute_case_id, item_no, id, fiscal_year, journal_entry_id, line_no,
		account_id, document_number, disputed_amount, disputed_amount_lc, open)
	SELECT tenant_id, id, 1, gen_random_uuid(), fiscal_year, journal_entry_id, line_no, gen_random_uuid(), document_number, disputed_amount,
		disputed_amount_lc, status = 'open'
	FROM collections.dispute_case d WHERE NOT EXISTS (SELECT FROM collections.dispute_case_item i WHERE i.tenant_id = d.tenant_id AND i.dispute_case_id = d.id)`

// TestDisputeCaseRules (T86a, migration 000324; ADR-0065 As built): a case
// names its customer, item and reason, disputes an amount above zero in a
// known currency, and is open, accepted or rejected, a closed one with its
// reason, time and person and an open one with none; an item has one open
// case, and another once that one is closed; the application assigns,
// notes on and closes an open case, and changes nothing else of it, and
// deletes none.
func TestDisputeCaseRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, id, entry uuid.UUID }
	open := func(s scene) stmt { return step(disputeCase, s.tenant, s.id, s.entry) }
	with := func(s scene, from, to string) stmt {
		return step(strings.Replace(disputeCase, from, to, 1), s.tenant, s.id, s.entry)
	}
	const closeIt = `UPDATE collections.dispute_case SET status = 'rejected', close_reason = 'Quoted', closed_at = now(), closed_by = $1,
		version = version + 1, updated_by = $1 WHERE id = $2`
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"an open case", func(s scene) []stmt { return []stmt{open(s)} }, ""},
		{"no customer number", func(s scene) []stmt { return []stmt{with(s, "'BP1'", "''")} }, "dispute_case_customer_number_check"},
		{"line 0", func(s scene) []stmt { return []stmt{with(s, "$3, 1, 'DR1'", "$3, 0, 'DR1'")} }, "dispute_case_line_no_check"},
		{"no document number", func(s scene) []stmt { return []stmt{with(s, "'DR1'", "''")} }, "dispute_case_document_number_check"},
		{"an unknown currency", func(s scene) []stmt { return []stmt{with(s, "'DR1', 'EUR'", "'DR1', 'XXA'")} }, "dispute_case_currency_fkey"},
		{"an unknown company currency", func(s scene) []stmt { return []stmt{with(s, "100, 'EUR', 100", "100, 'XXA', 100")} }, "dispute_case_currency_lc_fkey"},
		{"nothing disputed", func(s scene) []stmt { return []stmt{with(s, "'EUR', 100, 'EUR'", "'EUR', 0, 'EUR'")} }, "dispute_case_amount_check"},
		{"nothing in the company's currency", func(s scene) []stmt { return []stmt{with(s, "'EUR', 100, 'Price'", "'EUR', 0, 'Price'")} }, ""},
		{"below zero in the company's currency", func(s scene) []stmt { return []stmt{with(s, "'EUR', 100, 'Price'", "'EUR', -1, 'Price'")} },
			"dispute_case_amount_lc_check"},
		{"no reason", func(s scene) []stmt { return []stmt{with(s, "'Price'", "''")} }, "dispute_case_reason_check"},
		{"a reason of white space", func(s scene) []stmt { return []stmt{with(s, "'Price'", "'  '")} }, "dispute_case_reason_check"},
		{"a reason of 501 characters", func(s scene) []stmt { return []stmt{with(s, "'Price'", "repeat('x', 501)")} }, "dispute_case_reason_check"},
		{"a reason of 500 characters", func(s scene) []stmt { return []stmt{with(s, "'Price'", "repeat('x', 500)")} }, ""},
		{"an empty note", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET note = '', version = version + 1 WHERE id = $1`, s.id)}
		}, "dispute_case_note_check"},
		{"a note of white space", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET note = ' ', version = version + 1 WHERE id = $1`, s.id)}
		}, "dispute_case_note_check"},
		{"a note of 2001 characters", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET note = repeat('x', 2001), version = version + 1 WHERE id = $1`, s.id)}
		}, "dispute_case_note_check"},
		{"a note of 2000 characters, and an assignee", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET note = repeat('x', 2000), assignee_id = $1, version = version + 1 WHERE id = $2`,
				s.tenant, s.id)}
		}, ""},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{open(s), step(strings.Replace(closeIt, "'rejected'", "'withdrawn'", 1), s.tenant, s.id)}
		}, "dispute_case_status_check"},
		{"a closed case without its reason", func(s scene) []stmt { return []stmt{with(s, "'open', $1", "'accepted', $1")} }, "dispute_case_closed_check"},
		{"an open case with a reason", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET close_reason = 'x', version = version + 1 WHERE id = $1`, s.id)}
		}, "dispute_case_closed_check"},
		{"a case closed without who closed it", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET status = 'accepted', close_reason = 'x', closed_at = now(),
				version = version + 1 WHERE id = $1`, s.id)}
		}, "dispute_case_closed_check"},
		{"a case closed", func(s scene) []stmt { return []stmt{open(s), step(closeIt, s.tenant, s.id)} }, ""},
		{"a close reason of white space", func(s scene) []stmt {
			return []stmt{open(s), step(strings.Replace(closeIt, "'Quoted'", "' '", 1), s.tenant, s.id)}
		}, "dispute_case_close_reason_check"},
		{"a close reason of 501 characters", func(s scene) []stmt {
			return []stmt{open(s), step(strings.Replace(closeIt, "'Quoted'", "repeat('x', 501)", 1), s.tenant, s.id)}
		}, "dispute_case_close_reason_check"},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(strings.Replace(strings.Replace(disputeCase, "status, created_by", "status, version, created_by", 1), "'open', $1", "'open', 0, $1", 1),
				s.tenant, s.id, s.entry)}
		}, "dispute_case_version_check"},
		{"a second open case of the item", func(s scene) []stmt { return []stmt{open(s), step(disputeCase, s.tenant, id(), s.entry)} },
			"dispute_case_one_open"},
		{"another open case once the first is closed", func(s scene) []stmt {
			return []stmt{open(s), step(closeIt, s.tenant, s.id), step(disputeCase, s.tenant, id(), s.entry)}
		}, ""},
		{"a closed case changed", func(s scene) []stmt {
			return []stmt{open(s), step(closeIt, s.tenant, s.id), step(`UPDATE collections.dispute_case SET note = 'x' WHERE id = $1`, s.id)}
		}, "a closed case is not changed"},
		{"a closed case opened again", func(s scene) []stmt {
			return []stmt{open(s), step(closeIt, s.tenant, s.id), step(`UPDATE collections.dispute_case SET status = 'open', close_reason = NULL,
				closed_at = NULL, closed_by = NULL WHERE id = $1`, s.id)}
		}, "a closed case is not changed"},
		{"the amount changed", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET disputed_amount = 1 WHERE id = $1`, s.id)}
		}, "permission denied"},
		{"the reason changed", func(s scene) []stmt {
			return []stmt{open(s), step(`UPDATE collections.dispute_case SET reason = 'x' WHERE id = $1`, s.id)}
		}, "permission denied"},
		{"a case deleted", func(s scene) []stmt { return []stmt{open(s), step(`DELETE FROM collections.dispute_case`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), id: id(), entry: id()}
			// Each case gets its one item, its own line (T86c, migration 000395), which the deferred check wants at commit.
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, append(c.run(s), step(disputeCaseItems))...) })
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

// TestDisputeCaseClosedForTheOwner (T86a, migration 000324): a closed case
// is not changed by any role, the table's owner included: the trigger,
// not the grant, keeps it.
func TestDisputeCaseClosedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, c := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(disputeCase, tenant, c, id()),
		step(`UPDATE collections.dispute_case SET status = 'accepted', close_reason = 'x', closed_at = now(), closed_by = $1, version = version + 1
			WHERE id = $2`, tenant, c)); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE collections.dispute_case SET disputed_amount = 1 WHERE id = $1`, c)
	failsWith(t, err, "a closed case is not changed")
}

// TestDunningRunDisputedItems (T86b, migration 000325): a run's disputed
// items are a list, empty by default, as the previous version's runs have
// it.
func TestDunningRunDisputedItems(t *testing.T) {
	need(t)
	const run = `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
		overdue_amount, created_by, updated_by%s) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, $1, $1%s)`
	for _, c := range []struct{ name, column, value, want string }{
		{"none given", "", "", ""},
		{"a list", ", disputed_items", `, '[{"dispute_case_id": "x"}]'`, ""},
		{"an object", ", disputed_items", `, '{}'`, "dunning_run_disputed_items_check"},
		{"null", ", disputed_items", ", NULL", "disputed_items"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant, r := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				if err := all(tx, step(fmt.Sprintf(run, c.column, c.value), tenant, r)); err != nil {
					return err
				}
				var got string
				if err := tx.QueryRow(context.Background(), `SELECT disputed_items::text FROM collections.dunning_run WHERE id = $1`, r).Scan(&got); err != nil {
					return err
				}
				if c.column == "" && got != "[]" {
					t.Errorf("default: got %s, want []", got)
				}
				return nil
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

// TestDunningCharges (T76d, migration 000353): a run's grace days are 0 to
// 365, its minimum amount, charges and interest are not negative, its
// interest rate above 0 and below 100 percent; a run counts notices below
// the minimum only with a minimum, and an amount below it only with
// notices (which may amount to 0); a notice's
// charge and interest and an item's interest are not negative. Each
// defaults to none, as the previous version's rows have it, and the
// application's update grant leaves them out.
func TestDunningCharges(t *testing.T) {
	need(t)
	const run = `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
		overdue_amount, created_by, updated_by%s) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 1, 1, 10000, $1, $1%s)`
	const notice = `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count,
		overdue_amount%s) VALUES ($1, $3, $2, gen_random_uuid(), 'BP1', 1, 1, 10000%s)`
	const item = `INSERT INTO collections.dunning_notice_item (tenant_id, dunning_run_id, dunning_notice_id, item_no, fiscal_year, journal_entry_id,
		line_no, document_number, document_date, net_due_date, days_overdue, currency, amount, amount_lc, previous_level, level, previous_dunnings%s)
		VALUES ($1, $2, $3, 1, 2026, gen_random_uuid(), 1, 'DR1', '2026-03-01', '2026-03-31', 91, 'EUR', 10000, 10000, 0, 1, 0%s)`
	for _, c := range []struct{ name, runCols, runVals, noticeCols, noticeVals, itemCols, itemVals, want string }{
		{"none given", "", "", "", "", "", "", ""},
		{"all given", ", grace_days, minimum_amount, interest_rate_percent, charge_amount, interest_amount, below_minimum_count, below_minimum_amount",
			", 365, 2500, 99.999, 1500, 248, 1, 1250", ", charge_amount, interest_amount", ", 1500, 248", ", interest_amount", ", 248", ""},
		{"negative grace days", ", grace_days", ", -1", "", "", "", "", "dunning_run_grace_days_check"},
		{"more than a year's grace", ", grace_days", ", 366", "", "", "", "", "dunning_run_grace_days_check"},
		{"a negative minimum", ", minimum_amount", ", -1", "", "", "", "", "dunning_run_minimum_amount_check"},
		{"a rate of 0", ", interest_rate_percent", ", 0", "", "", "", "", "dunning_run_interest_rate_check"},
		{"a rate of 100", ", interest_rate_percent", ", 100", "", "", "", "", "dunning_run_interest_rate_check"},
		{"a rate of four places", ", interest_rate_percent", ", 9.1234", "", "", "", "", ""},
		{"a negative charge", ", charge_amount", ", -1", "", "", "", "", "dunning_run_charge_amount_check"},
		{"negative interest", ", interest_amount", ", -1", "", "", "", "", "dunning_run_interest_amount_check"},
		{"a negative count below the minimum", ", minimum_amount, below_minimum_count, below_minimum_amount", ", 100, -1, 0", "", "", "", "", "dunning_run_below_minimum_count_check"},
		{"a negative amount below the minimum", ", minimum_amount, below_minimum_count, below_minimum_amount", ", 100, 1, -1", "", "", "", "", "dunning_run_below_minimum_amount_check"},
		{"a notice below the minimum amounting to 0", ", minimum_amount, below_minimum_count", ", 100, 1", "", "", "", "", ""},
		{"an amount below the minimum without notices", ", minimum_amount, below_minimum_amount", ", 100, 1", "", "", "", "",
			"dunning_run_below_minimum_check"},
		{"notices below no minimum", ", below_minimum_count, below_minimum_amount", ", 1, 100", "", "", "", "", "dunning_run_below_minimum_check"},
		{"a notice's negative charge", "", "", ", charge_amount", ", -1", "", "", "dunning_notice_charge_amount_check"},
		{"a notice's negative interest", "", "", ", interest_amount", ", -1", "", "", "dunning_notice_interest_amount_check"},
		{"an item's negative interest", "", "", "", "", ", interest_amount", ", -1", "dunning_notice_item_interest_amount_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant, r, n := id(), id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				if err := all(tx, step(fmt.Sprintf(run, c.runCols, c.runVals), tenant, r), step(fmt.Sprintf(notice, c.noticeCols, c.noticeVals), tenant, r, n),
					step(fmt.Sprintf(item, c.itemCols, c.itemVals), tenant, r, n)); err != nil {
					return err
				}
				var got string
				if err := tx.QueryRow(context.Background(), `SELECT concat_ws(' ', r.grace_days, r.minimum_amount, r.interest_rate_percent, r.charge_amount,
						r.interest_amount, r.below_minimum_count, r.below_minimum_amount, n.charge_amount, n.interest_amount, i.interest_amount)
					FROM collections.dunning_run r JOIN collections.dunning_notice n ON n.dunning_run_id = r.id
					JOIN collections.dunning_notice_item i ON i.dunning_notice_id = n.id WHERE r.id = $1`, r).Scan(&got); err != nil {
					return err
				}
				if c.runCols == "" && c.noticeCols == "" && c.itemCols == "" && got != "0 0 0 0 0 0 0 0 0" {
					t.Errorf("defaults: got %s, want 0 0 0 0 0 0 0 0 0, no rate", got)
				}
				return nil
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
	t.Run("the application changes no charge", func(t *testing.T) {
		tenant, r, n := id(), id(), id()
		err := asApp(t, tenant, func(tx pgx.Tx) error {
			return all(tx, step(fmt.Sprintf(run, "", ""), tenant, r), step(fmt.Sprintf(notice, "", ""), tenant, r, n),
				step(`UPDATE collections.dunning_run SET charge_amount = 1, version = version + 1`))
		})
		failsWith(t, err, "permission denied")
	})
}

// dunningBlock places a block of item $3's line 1 for tenant $1, id $2.
const dunningBlock = `INSERT INTO collections.dunning_block (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year, journal_entry_id,
	line_no, document_number, reason, valid_to, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 2026, $3, 1, 'DR1', 'Delivery checked', '2027-01-31', $1, $1)`

// TestDunningBlockRules (T76e, migration 000354): a block names its
// customer by number and an item by its whole journal line with its
// document number, or none of them; its reason is more than white space
// and at most 500 characters; a released block has when and by whom; the
// application releases a block and changes nothing else of it, once;
// it deletes none. A run's blocked items are a list, empty by default.
func TestDunningBlockRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, id, entry uuid.UUID }
	place := func(s scene) stmt { return step(dunningBlock, s.tenant, s.id, s.entry) }
	with := func(s scene, from, to string) stmt {
		return step(strings.Replace(dunningBlock, from, to, 1), s.tenant, s.id, s.entry)
	}
	const release = `UPDATE collections.dunning_block SET released_at = now(), released_by = $1, version = version + 1, updated_by = $1 WHERE id = $2`
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"an item's block", func(s scene) []stmt { return []stmt{place(s)} }, ""},
		{"a customer's block", func(s scene) []stmt {
			return []stmt{with(s, "2026, $3, 1, 'DR1'", "NULL, NULLIF($3::uuid, $3::uuid), NULL, NULL")}
		}, ""},
		{"no customer number", func(s scene) []stmt { return []stmt{with(s, "'BP1'", "''")} }, "dunning_block_customer_number_check"},
		{"line 0", func(s scene) []stmt { return []stmt{with(s, "$3, 1, 'DR1'", "$3, 0, 'DR1'")} }, "dunning_block_line_no_check"},
		{"no document number", func(s scene) []stmt { return []stmt{with(s, "'DR1'", "''")} }, "dunning_block_document_number_check"},
		{"a line without its year", func(s scene) []stmt { return []stmt{with(s, "2026, $3", "NULL, $3")} }, "dunning_block_item_check"},
		{"a year alone", func(s scene) []stmt {
			return []stmt{with(s, "2026, $3, 1, 'DR1'", "2026, NULLIF($3::uuid, $3::uuid), NULL, NULL")}
		},
			"dunning_block_item_check"},
		{"a line without its document number", func(s scene) []stmt { return []stmt{with(s, "'DR1'", "NULL")} }, "dunning_block_item_check"},
		{"a reason of white space", func(s scene) []stmt { return []stmt{with(s, "'Delivery checked'", "'  '")} }, "dunning_block_reason_check"},
		{"a reason of 501 characters", func(s scene) []stmt { return []stmt{with(s, "'Delivery checked'", "repeat('x', 501)")} }, "dunning_block_reason_check"},
		{"a reason of 500 characters", func(s scene) []stmt { return []stmt{with(s, "'Delivery checked'", "repeat('x', 500)")} }, ""},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(strings.Replace(strings.Replace(dunningBlock, "valid_to, created_by", "valid_to, version, created_by", 1),
				"'2027-01-31', $1", "'2027-01-31', 0, $1", 1), s.tenant, s.id, s.entry)}
		}, "dunning_block_version_check"},
		{"released", func(s scene) []stmt { return []stmt{place(s), step(release, s.tenant, s.id)} }, ""},
		{"released without by whom", func(s scene) []stmt {
			return []stmt{place(s), step(`UPDATE collections.dunning_block SET released_at = now(), version = version + 1 WHERE id = $1`, s.id)}
		}, "dunning_block_released_check"},
		{"released twice", func(s scene) []stmt {
			return []stmt{place(s), step(release, s.tenant, s.id), step(release, s.tenant, s.id)}
		},
			"a released block is not changed"},
		{"the end date changed", func(s scene) []stmt {
			return []stmt{place(s), step(`UPDATE collections.dunning_block SET valid_to = '2027-12-31' WHERE id = $1`, s.id)}
		}, "permission denied"},
		{"the reason changed", func(s scene) []stmt {
			return []stmt{place(s), step(`UPDATE collections.dunning_block SET reason = 'x' WHERE id = $1`, s.id)}
		}, "permission denied"},
		{"a block deleted", func(s scene) []stmt { return []stmt{place(s), step(`DELETE FROM collections.dunning_block`)} }, "permission denied"},
		{"a run's blocked items an object", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
				overdue_amount, blocked_items, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, '{}', $1, $1)`,
				s.tenant, s.id)}
		}, "dunning_run_blocked_items_check"},
		{"a run's blocked items by default", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
				overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, $1, $1)`, s.tenant, s.id),
				step(`DO $$ BEGIN IF (SELECT blocked_items FROM collections.dunning_run) <> '[]'::jsonb THEN RAISE 'not empty'; END IF; END $$`)}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), id: id(), entry: id()}
			// Each case gets its one item, its own line (T86c, migration 000395), which the deferred check wants at commit.
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, append(c.run(s), step(disputeCaseItems))...) })
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

// TestDunningBlockReleasedForTheOwner (T76e, migration 000354): a
// released block is not changed by any role, the table's owner included:
// the trigger, not the grant, keeps it.
func TestDunningBlockReleasedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, b := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(dunningBlock, tenant, b, id()),
		step(`UPDATE collections.dunning_block SET released_at = now(), released_by = $1, version = version + 1 WHERE id = $2`, tenant, b)); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE collections.dunning_block SET valid_to = '2099-12-31' WHERE id = $1`, b)
	failsWith(t, err, "a released block is not changed")
}

// promiseToPay records a promise of tenant $1, id $2, by 2027-01-31.
const promiseToPay = `INSERT INTO collections.promise_to_pay (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, promised_amount,
	promised_date, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 'EUR', 10000, '2027-01-31', $1, $1)`

// promiseToPayItem is line 1 of entry $3 in promise $2 of tenant $1.
const promiseToPayItem = `INSERT INTO collections.promise_to_pay_item (tenant_id, promise_to_pay_id, fiscal_year, journal_entry_id, line_no,
	document_number, net_due_date, currency, amount, amount_lc) VALUES ($1, $2, 2026, $3, 1, 'DR1', '2026-12-31', 'EUR', 10000, 10000)`

// TestPromiseToPayRules (T76f, migration 000365): a promise names its
// customer by number, promises an amount above zero in a known currency
// by a date up to 2100-12-31, at version 1 or more; an item is named by
// its journal line, once in its promise, with its document number and an
// amount above zero, of a promise that exists; the application changes
// and deletes neither. A run's promised items are a list, empty by
// default.
func TestPromiseToPayRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, id, entry uuid.UUID }
	promise := func(s scene) stmt { return step(promiseToPay, s.tenant, s.id) }
	promiseWith := func(s scene, from, to string) stmt {
		return step(strings.Replace(promiseToPay, from, to, 1), s.tenant, s.id)
	}
	item := func(s scene) stmt { return step(promiseToPayItem, s.tenant, s.id, s.entry) }
	itemWith := func(s scene, from, to string) stmt {
		return step(strings.Replace(promiseToPayItem, from, to, 1), s.tenant, s.id, s.entry)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a promise with an item", func(s scene) []stmt { return []stmt{promise(s), item(s)} }, ""},
		{"no customer number", func(s scene) []stmt { return []stmt{promiseWith(s, "'BP1'", "''")} }, "promise_to_pay_customer_number_check"},
		{"an unknown currency", func(s scene) []stmt { return []stmt{promiseWith(s, "'EUR'", "'XXA'")} }, "promise_to_pay_currency_fkey"},
		{"an amount of zero", func(s scene) []stmt { return []stmt{promiseWith(s, "10000, '2027", "0, '2027")} }, "promise_to_pay_amount_positive"},
		{"a date after 2100", func(s scene) []stmt { return []stmt{promiseWith(s, "'2027-01-31'", "'2101-01-01'")} }, "promise_to_pay_date_check"},
		{"the last date", func(s scene) []stmt { return []stmt{promiseWith(s, "'2027-01-31'", "'2100-12-31'")} }, ""},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(strings.Replace(strings.Replace(promiseToPay, "promised_date, created_by", "promised_date, version, created_by", 1),
				"'2027-01-31', $1", "'2027-01-31', 0, $1", 1), s.tenant, s.id)}
		}, "promise_to_pay_version_check"},
		{"an item of no promise", func(s scene) []stmt { return []stmt{item(s)} }, "promise_to_pay_item_tenant_id_promise_to_pay_id_fkey"},
		{"an item twice", func(s scene) []stmt { return []stmt{promise(s), item(s), item(s)} }, "promise_to_pay_item_pkey"},
		{"line 0", func(s scene) []stmt { return []stmt{promise(s), itemWith(s, "$3, 1,", "$3, 0,")} }, "promise_to_pay_item_line_no_check"},
		{"no document number", func(s scene) []stmt { return []stmt{promise(s), itemWith(s, "'DR1'", "''")} },
			"promise_to_pay_item_document_number_check"},
		{"an item of an unknown currency", func(s scene) []stmt { return []stmt{promise(s), itemWith(s, "'EUR'", "'XXA'")} },
			"promise_to_pay_item_currency_fkey"},
		{"a credit item", func(s scene) []stmt { return []stmt{promise(s), itemWith(s, "'EUR', 10000", "'EUR', -1")} },
			"promise_to_pay_item_amount_positive"},
		{"a negative local amount", func(s scene) []stmt { return []stmt{promise(s), itemWith(s, "10000, 10000)", "10000, -1)")} },
			"promise_to_pay_item_amount_lc_check"},
		{"the promise changed", func(s scene) []stmt {
			return []stmt{promise(s), step(`UPDATE collections.promise_to_pay SET promised_date = '2027-12-31' WHERE id = $1`, s.id)}
		}, "permission denied"},
		{"the promise deleted", func(s scene) []stmt { return []stmt{promise(s), step(`DELETE FROM collections.promise_to_pay`)} }, "permission denied"},
		{"an item changed", func(s scene) []stmt {
			return []stmt{promise(s), item(s), step(`UPDATE collections.promise_to_pay_item SET amount = 1`)}
		}, "permission denied"},
		{"an item deleted", func(s scene) []stmt {
			return []stmt{promise(s), item(s), step(`DELETE FROM collections.promise_to_pay_item`)}
		}, "permission denied"},
		{"a run's promised items an object", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
				overdue_amount, promised_items, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, '{}', $1, $1)`,
				s.tenant, s.id)}
		}, "dunning_run_promised_items_check"},
		{"a run's promised items by default", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
				overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, $1, $1)`, s.tenant, s.id),
				step(`DO $$ BEGIN IF (SELECT promised_items FROM collections.dunning_run) <> '[]'::jsonb THEN RAISE 'not empty'; END IF; END $$`)}
		}, ""},
		{"a run's promised items changed", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
				overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 0, 0, 0, $1, $1)`, s.tenant, s.id),
				step(`UPDATE collections.dunning_run SET promised_items = '[{}]', version = version + 1`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), id: id(), entry: id()}
			// Each case gets its one item, its own line (T86c, migration 000395), which the deferred check wants at commit.
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, append(c.run(s), step(disputeCaseItems))...) })
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

// TestPromiseToPayFixedForTheOwner (T76f, migration 000365): a promise and
// its items are insert-only for every role, the tables' owner included:
// the trigger, not the grant, keeps them.
func TestPromiseToPayFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, p, entry := id(), id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(promiseToPay, tenant, p),
		step(promiseToPayItem, tenant, p, entry)); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"UPDATE collections.promise_to_pay SET promised_amount = 1 WHERE tenant_id = $1",
		"DELETE FROM collections.promise_to_pay WHERE tenant_id = $1", "UPDATE collections.promise_to_pay_item SET amount = 1 WHERE tenant_id = $1",
		"DELETE FROM collections.promise_to_pay_item WHERE tenant_id = $1"} {
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

// customerContact records a contact of tenant $1, id $2.
const customerContact = `INSERT INTO collections.customer_contact (tenant_id, id, legal_entity_id, customer_id, customer_number, contacted_at,
	contact_name, outcome, note, resubmission_date, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', now(), 'Dana', 'reached', 'Pays Friday', '2027-01-31', $1, $1)`

// TestCustomerContactRules (T76g, migration 000366): a contact names its
// customer by number; its outcome is reached, not_reached or
// left_message; its contact name and note are more than white space, at
// most 200 and 2,000 characters, or absent; its resubmission date is up
// to 2100-12-31 or absent; version from 1; the application changes and
// deletes none.
func TestCustomerContactRules(t *testing.T) {
	need(t)
	with := func(tenant, c uuid.UUID, from, to string) stmt {
		return step(strings.Replace(customerContact, from, to, 1), tenant, c)
	}
	cases := []struct {
		name     string
		from, to string
		then     string
		want     string // empty: allowed
	}{
		{"a contact", "", "", "", ""},
		{"no customer number", "'BP1'", "''", "", "customer_contact_customer_number_check"},
		{"an unknown outcome", "'reached'", "'emailed'", "", "customer_contact_outcome_check"},
		{"not reached", "'reached'", "'not_reached'", "", ""},
		{"a message left", "'reached'", "'left_message'", "", ""},
		{"a contact name of white space", "'Dana'", "' '", "", "customer_contact_contact_name_check"},
		{"a contact name of 201 characters", "'Dana'", "repeat('x', 201)", "", "customer_contact_contact_name_check"},
		{"a contact name of 200 characters", "'Dana'", "repeat('x', 200)", "", ""},
		{"no contact name, note or resubmission", "'Dana', 'reached', 'Pays Friday', '2027-01-31'", "NULL, 'reached', NULL, NULL", "", ""},
		{"a note of white space", "'Pays Friday'", "' '", "", "customer_contact_note_check"},
		{"a note of 2001 characters", "'Pays Friday'", "repeat('x', 2001)", "", "customer_contact_note_check"},
		{"a resubmission after 2100", "'2027-01-31'", "'2101-01-01'", "", "customer_contact_resubmission_date_check"},
		{"changed", "", "", `UPDATE collections.customer_contact SET outcome = 'not_reached'`, "permission denied"},
		{"deleted", "", "", `DELETE FROM collections.customer_contact`, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, id := id(), id()
			stmts := []stmt{step(customerContact, tenant, id)}
			if c.from != "" {
				stmts = []stmt{with(tenant, id, c.from, c.to)}
			}
			if c.then != "" {
				stmts = append(stmts, step(c.then))
			}
			err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, stmts...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("version 0", func(t *testing.T) {
		tenant := id()
		sql := strings.Replace(strings.Replace(customerContact, "resubmission_date, created_by", "resubmission_date, version, created_by", 1),
			"'2027-01-31', $1", "'2027-01-31', 0, $1", 1)
		failsWith(t, asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, step(sql, tenant, id())) }), "customer_contact_version_check")
	})
}

// TestCustomerContactFixedForTheOwner (T76g, migration 000366): a contact
// is insert-only for every role, the table's owner included.
func TestCustomerContactFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(customerContact, tenant, id())); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"UPDATE collections.customer_contact SET note = 'x' WHERE tenant_id = $1",
		"DELETE FROM collections.customer_contact WHERE tenant_id = $1"} {
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
