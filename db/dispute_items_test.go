package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// disputeCaseOf is a case of two items, $3 and $4 (line 1 of each entry),
// disputing 30 and 70 of them, the first its own line.
const disputeCaseOf = `INSERT INTO collections.dispute_case (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year, journal_entry_id,
	line_no, document_number, currency, disputed_amount, currency_lc, disputed_amount_lc, reason, status, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 2026, $3, 1, 'DR1', 'EUR', 100, 'EUR', 100, 'Late', 'open', $1, $1)`

// disputeItem is item $3 of case $2 on line 1 of entry $4, disputing $5
// (and as much in the company's currency).
const disputeItem = `INSERT INTO collections.dispute_case_item (tenant_id, dispute_case_id, item_no, id, fiscal_year, journal_entry_id, line_no,
	document_number, disputed_amount, disputed_amount_lc) VALUES ($1, $2, $3, gen_random_uuid(), 2026, $4, 1, 'DR1', $5, $5)`

// TestDisputeCaseItemRules (T86c, migration 000395; ADR-0065 As built): a
// case's items are numbered 1 to 50, name a line once in the case, dispute
// more than zero of it, and are of an existing case; an item is in one open
// case at a time, and in another once that one is closed; at commit a case
// has items, its own line its first item's and its amounts their totals,
// each open while the case is; the application changes and deletes no
// item.
func TestDisputeCaseItemRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, id, a, b uuid.UUID }
	open := func(s scene) []stmt {
		return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30), step(disputeItem, s.tenant, s.id, 2, s.b, 70)}
	}
	// with is the case with its second item changed.
	with := func(s scene, from, to string) []stmt {
		return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30),
			step(strings.Replace(disputeItem, from, to, 1), s.tenant, s.id, 2, s.b, 70)}
	}
	const closeIt = `UPDATE collections.dispute_case SET status = 'rejected', close_reason = 'Late', closed_at = now(), closed_by = $1,
		version = version + 1, updated_by = $1 WHERE id = $2`
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a case of two items", open, ""},
		{"item 0", func(s scene) []stmt { return with(s, "$1, $2, $3,", "$1, $2, $3 - 2,") }, "dispute_case_item_no_check"},
		{"item 51", func(s scene) []stmt { return with(s, "$1, $2, $3,", "$1, $2, $3 + 49,") }, "dispute_case_item_no_check"},
		{"line 0", func(s scene) []stmt { return with(s, "$4, 1, 'DR1'", "$4, 0, 'DR1'") }, "dispute_case_item_line_no_check"},
		{"no document number", func(s scene) []stmt { return with(s, "'DR1'", "''") }, "dispute_case_item_document_number_check"},
		{"nothing disputed", func(s scene) []stmt { return with(s, "'DR1', $5, $5", "'DR1', 0, $5") }, "dispute_case_item_amount_check"},
		{"below zero in the company's currency", func(s scene) []stmt { return with(s, "'DR1', $5, $5", "'DR1', $5, -1") }, "dispute_case_item_amount_lc_check"},
		{"a line twice in the case", func(s scene) []stmt {
			return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30), step(disputeItem, s.tenant, s.id, 2, s.a, 70)}
		}, "dispute_case_item_once"},
		{"an item of no case", func(s scene) []stmt { return []stmt{step(disputeItem, s.tenant, id(), 1, s.a, 30)} }, "dispute_case_item_case_fkey"},
		{"an item of an open case in another", func(s scene) []stmt {
			other := id()
			return append(open(s), step(disputeCaseOf, s.tenant, other, id()), step(disputeItem, s.tenant, other, 1, id(), 30),
				step(disputeItem, s.tenant, other, 2, s.b, 70))
		}, "dispute_case_item_one_open"},
		{"the item in another case once the first is closed", func(s scene) []stmt {
			other, first := id(), id()
			return append(open(s), step(closeIt, s.tenant, s.id), step(disputeCaseOf, s.tenant, other, first), step(disputeItem, s.tenant, other, 1, first, 30),
				step(disputeItem, s.tenant, other, 2, s.b, 70))
		}, ""},
		{"a case without items", func(s scene) []stmt { return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a)} }, "its items are its amounts"},
		{"items that are not the case's amount", func(s scene) []stmt {
			return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30), step(disputeItem, s.tenant, s.id, 2, s.b, 69)}
		}, "its items are its amounts"},
		{"items that are not the case's local amount", func(s scene) []stmt { return with(s, "$5, $5)", "$5, 69)") }, "its items are its amounts"},
		{"a first item that is not the case's line", func(s scene) []stmt {
			return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.b, 30), step(disputeItem, s.tenant, s.id, 2, s.a, 70)}
		}, "its items are its amounts"},
		{"an open item of a closed case", func(s scene) []stmt {
			closed := strings.Replace(strings.Replace(disputeCaseOf, "status, created_by", "status, close_reason, closed_at, closed_by, created_by", 1),
				"'open', $1", "'rejected', 'Late', now(), $1, $1", 1)
			return []stmt{step(closed, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30), step(disputeItem, s.tenant, s.id, 2, s.b, 70)}
		}, "its items are its amounts"},
		{"an item changed", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET disputed_amount = 1 WHERE dispute_case_id = $1`, s.id))
		}, "permission denied"},
		{"an item deleted", func(s scene) []stmt {
			return append(open(s), step(`DELETE FROM collections.dispute_case_item WHERE dispute_case_id = $1`, s.id))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), id: id(), a: id(), b: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestDisputeCaseItemsLater (T86c, migration 000395): an item added to
// a committed case changes its total, which the deferred check refuses;
// closing the case closes its items, which no role reopens, changes or
// deletes, the table's owner included: the trigger, not the grant, keeps
// them.
func TestDisputeCaseItemsLater(t *testing.T) {
	need(t)
	tenant, c, a, b := id(), id(), id(), id()
	mustApp(t, tenant, func(tx pgx.Tx) error {
		return all(tx, step(disputeCaseOf, tenant, c, a), step(disputeItem, tenant, c, 1, a, 30), step(disputeItem, tenant, c, 2, b, 70))
	})
	err := asApp(t, tenant, func(tx pgx.Tx) error { return exec(tx, disputeItem, tenant, c, 3, id(), 10) })
	failsWith(t, err, "its items are its amounts")
	mustApp(t, tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE collections.dispute_case SET status = 'accepted', close_reason = 'Late', closed_at = now(), closed_by = $1,
			version = version + 1, updated_by = $1 WHERE id = $2`, tenant, c)
	})
	var open int
	if err := testDB.QueryRow(context.Background(), `SELECT count(*) FROM collections.dispute_case_item WHERE dispute_case_id = $1 AND open`, c).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Errorf("open items of the closed case: got %d, want 0", open)
	}
	for name, sql := range map[string]string{
		"reopened": `UPDATE collections.dispute_case_item SET open = true WHERE dispute_case_id = $1`,
		"changed":  `UPDATE collections.dispute_case_item SET disputed_amount = 1 WHERE dispute_case_id = $1`,
		"deleted":  `DELETE FROM collections.dispute_case_item WHERE dispute_case_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := testDB.Exec(context.Background(), sql, c)
			failsOn(t, err, "dispute_case_item_fixed")
		})
	}
}

// TestDisputeCaseItemsBackfilled: 000395 gives each case opened before it
// its one item, its own line, open while the case is.
func TestDisputeCaseItemsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000395_dispute_case_items")
	tenant, open, closed, a, b := id(), id(), id(), id(), id()
	inTenant(t, conn, tenant, func(tx pgx.Tx) error {
		return all(tx, step(strings.Replace(disputeCaseOf, "'DR1'", "'DR7'", 1), tenant, open, a),
			step(strings.Replace(strings.Replace(disputeCaseOf, "status, created_by", "status, close_reason, closed_at, closed_by, created_by", 1),
				"'open', $1", "'rejected', 'Late', now(), $1, $1", 1), tenant, closed, b))
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(context.Background(), `SELECT (i.dispute_case_id = $2)::text || ' ' || i.item_no || ' ' || (i.journal_entry_id = d.journal_entry_id)::text
			|| ' ' || i.document_number || ' ' || i.disputed_amount || ' ' || i.disputed_amount_lc || ' ' || i.open
		FROM collections.dispute_case_item i JOIN collections.dispute_case d ON d.tenant_id = i.tenant_id AND d.id = i.dispute_case_id
		WHERE i.tenant_id = $1 ORDER BY i.open DESC`, tenant, open)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if g, want := strings.Join(got, ", "), "true 1 true DR7 100 100 true, false 1 true DR1 100 100 false"; g != want {
		t.Errorf("the items: got %s, want %s", g, want)
	}
}
