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
	account_id, document_number, disputed_amount, disputed_amount_lc) VALUES ($1, $2, $3, gen_random_uuid(), 2026, $4, 1, gen_random_uuid(), 'DR1', $5, $5)`

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
		{"line 0", func(s scene) []stmt { return with(s, "$4, 1, gen_random_uuid()", "$4, 0, gen_random_uuid()") }, "dispute_case_item_line_no_check"},
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
// its one item, its own line on the line's account, open while the case
// is.
func TestDisputeCaseItemsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000395_dispute_case_items")
	f := fixtureIDs()
	open, closed := id(), id()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		var entries [2]uuid.UUID
		for i := range entries {
			var err error
			if entries[i], err = f.entry(tx, 2, "2026-02-10", line{account: f.bank, amount: 100}, line{account: f.expense, amount: -100}); err != nil {
				return err
			}
		}
		return all(tx, step(strings.Replace(disputeCaseOf, "'DR1'", "'DR7'", 1), f.tenant, open, entries[0]),
			step(strings.Replace(strings.Replace(disputeCaseOf, "status, created_by", "status, close_reason, closed_at, closed_by, created_by", 1),
				"'open', $1", "'rejected', 'Late', now(), $1, $1", 1), f.tenant, closed, entries[1]))
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(context.Background(), `SELECT (i.dispute_case_id = $2)::text || ' ' || i.item_no || ' ' || (i.journal_entry_id = d.journal_entry_id)::text
			|| ' ' || (i.account_id = $3)::text || ' ' || i.document_number || ' ' || i.disputed_amount || ' ' || i.disputed_amount_lc || ' ' || i.open
		FROM collections.dispute_case_item i JOIN collections.dispute_case d ON d.tenant_id = i.tenant_id AND d.id = i.dispute_case_id
		WHERE i.tenant_id = $1 ORDER BY i.open DESC`, f.tenant, open, f.bank)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if g, want := strings.Join(got, ", "), "true 1 true true DR7 100 100 true, false 1 true true DR1 100 100 false"; g != want {
		t.Errorf("the items: got %s, want %s", g, want)
	}
}

// TestDisputeCaseResolutionRules (T86d, migration 000396; ADR-0065 As
// built): only an accepted case has a resolution, a known one, accepting
// more than zero and at most what it disputes, with its local amount and
// posting date; a write-off names its entry and a credit memo resolution
// none; an item's accepted amount is from zero to what it disputes, with
// its local amount, and a credit memo names it with its number only when
// it accepted some; at commit a resolved case's items each have what was
// accepted of them, together its accepted amounts, a credit memo exactly
// on each item a credit memo resolution accepted, and an unresolved case
// has none; an item's part is written once, while its case is open.
func TestDisputeCaseResolutionRules(t *testing.T) {
	need(t)
	type scene struct{ tenant, id, a, b uuid.UUID }
	open := func(s scene) []stmt {
		return []stmt{step(disputeCaseOf, s.tenant, s.id, s.a), step(disputeItem, s.tenant, s.id, 1, s.a, 30), step(disputeItem, s.tenant, s.id, 2, s.b, 70)}
	}
	// accept writes what is accepted of item no, and its credit memo when memo.
	accept := func(s scene, no, amount int, memo bool) stmt {
		sql := `UPDATE collections.dispute_case_item SET accepted_amount = $3, accepted_amount_lc = $3 WHERE dispute_case_id = $1 AND item_no = $2`
		if memo {
			sql = strings.Replace(sql, "accepted_amount_lc = $3", "accepted_amount_lc = $3, credit_memo_id = gen_random_uuid(), credit_memo_number = 'CM1'", 1)
		}
		return step(sql, s.id, no, amount)
	}
	const resolve = `UPDATE collections.dispute_case SET status = 'accepted', close_reason = 'Late', closed_at = now(), closed_by = $1, resolution = $3,
		accepted_amount = $4, accepted_amount_lc = $4, posting_date = '2026-06-30', write_off_entry_year = $5, write_off_entry_id = $6,
		write_off_entry_number = $7, version = version + 1 WHERE id = $2`
	byMemo := func(s scene, amount int) stmt {
		return step(resolve, s.tenant, s.id, "credit_memo", amount, nil, nil, nil)
	}
	byWriteOff := func(s scene, amount int) stmt {
		return step(resolve, s.tenant, s.id, "write_off", amount, 2026, id(), "100001")
	}
	with := func(s scene, from, to string, args ...any) stmt {
		return step(strings.Replace(resolve, from, to, 1), append([]any{s.tenant, s.id}, args...)...)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"resolved by credit memo, in part", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, true), accept(s, 2, 0, false), byMemo(s, 30))
		}, ""},
		{"resolved by write-off", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, false), accept(s, 2, 70, false), byWriteOff(s, 100))
		}, ""},
		{"an unknown resolution", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, false), accept(s, 2, 70, false), step(resolve, s.tenant, s.id, "refund", 100, nil, nil, nil))
		}, "dispute_case_resolution_check"},
		{"a resolution of an open case", func(s scene) []stmt {
			return append(open(s), with(s, "status = 'accepted', close_reason = 'Late', closed_at = now(), closed_by = $1,", "updated_by = $1,", "write_off", 1, 2026,
				id(), "1"))
		}, "dispute_case_resolved_check"},
		{"nothing accepted", func(s scene) []stmt { return append(open(s), byWriteOff(s, 0)) }, "dispute_case_resolved_check"},
		{"more than disputed", func(s scene) []stmt { return append(open(s), byWriteOff(s, 101)) }, "dispute_case_resolved_check"},
		{"no posting date", func(s scene) []stmt {
			return append(open(s), with(s, "posting_date = '2026-06-30'", "posting_date = NULL", "write_off", 100, 2026, id(), "1"))
		}, "dispute_case_resolved_check"},
		{"below zero locally", func(s scene) []stmt {
			return append(open(s), with(s, "accepted_amount_lc = $4", "accepted_amount_lc = -1", "write_off", 100, 2026, id(), "1"))
		}, "dispute_case_accepted_amount_lc_check"},
		{"a write-off without its entry", func(s scene) []stmt {
			return append(open(s), step(resolve, s.tenant, s.id, "write_off", 100, nil, nil, nil))
		},
			"dispute_case_write_off_check"},
		{"a credit memo resolution with an entry", func(s scene) []stmt {
			return append(open(s), step(resolve, s.tenant, s.id, "credit_memo", 100, 2026, id(), "1"))
		}, "dispute_case_write_off_check"},
		{"an entry without its number", func(s scene) []stmt {
			return append(open(s), step(resolve, s.tenant, s.id, "write_off", 100, 2026, id(), nil))
		},
			"dispute_case_write_off_check"},
		{"an entry number that is none", func(s scene) []stmt {
			return append(open(s), step(resolve, s.tenant, s.id, "write_off", 100, 2026, id(), ""))
		},
			"dispute_case_write_off_entry_number_check"},
		{"an item accepted above what it disputes", func(s scene) []stmt { return append(open(s), accept(s, 1, 31, false)) },
			"dispute_case_item_accepted_check"},
		{"an item accepted below zero", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET accepted_amount = -1, accepted_amount_lc = 0 WHERE dispute_case_id = $1
				AND item_no = 1`, s.id))
		}, "dispute_case_item_accepted_check"},
		{"an item accepted without its local amount", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET accepted_amount = 1 WHERE dispute_case_id = $1 AND item_no = 1`, s.id))
		}, "dispute_case_item_accepted_check"},
		{"an item accepted below zero locally", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET accepted_amount = 1, accepted_amount_lc = -1 WHERE dispute_case_id = $1
				AND item_no = 1`, s.id))
		}, "dispute_case_item_accepted_amount_lc_check"},
		{"a credit memo of an item accepted nothing", func(s scene) []stmt { return append(open(s), accept(s, 1, 0, true)) },
			"dispute_case_item_accepted_check"},
		{"a credit memo without its number", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET accepted_amount = 1, accepted_amount_lc = 1, credit_memo_id = gen_random_uuid()
				WHERE dispute_case_id = $1 AND item_no = 1`, s.id))
		}, "dispute_case_item_accepted_check"},
		{"a credit memo number that is none", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case_item SET accepted_amount = 1, accepted_amount_lc = 1, credit_memo_id = gen_random_uuid(),
				credit_memo_number = '' WHERE dispute_case_id = $1 AND item_no = 1`, s.id))
		}, "dispute_case_item_credit_memo_number_check"},
		{"items that are not what the case accepted", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, false), accept(s, 2, 70, false), byWriteOff(s, 99))
		}, "a resolved case's items are what it accepted"},
		{"an item not accepted at all", func(s scene) []stmt { return append(open(s), accept(s, 1, 30, false), byWriteOff(s, 30)) },
			"a resolved case's items are what it accepted"},
		{"an item a credit memo resolution accepted without its memo", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, false), accept(s, 2, 0, false), byMemo(s, 30))
		}, "a resolved case's items are what it accepted"},
		{"a credit memo of a write-off's item", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, true), accept(s, 2, 70, false), byWriteOff(s, 100))
		}, "a resolved case's items are what it accepted"},
		{"items accepted of a case not resolved", func(s scene) []stmt { return append(open(s), accept(s, 1, 30, false)) },
			"a resolved case's items are what it accepted"},
		{"an item's part written again", func(s scene) []stmt {
			return append(open(s), accept(s, 1, 30, false), accept(s, 1, 20, false))
		}, "its resolution is written once while its case is open"},
		{"an item's part written after the close", func(s scene) []stmt {
			return append(open(s), step(`UPDATE collections.dispute_case SET status = 'rejected', close_reason = 'Late', closed_at = now(), closed_by = $1,
				version = version + 1 WHERE id = $2`, s.tenant, s.id), accept(s, 1, 30, false))
		}, "its resolution is written once while its case is open"},
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

// TestCollectionsJournalSource (T86d, migrations 000396 and 000397): the
// journal takes entries of the collections context, a dispute case's
// write-off, and its check of source contexts is validated on every
// partition.
func TestCollectionsJournalSource(t *testing.T) {
	need(t)
	var def string
	var unvalidated int
	if err := testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid),
			(SELECT count(*) FROM pg_constraint WHERE conname = 'journal_entry_source_context_check' AND NOT convalidated)
		FROM pg_constraint WHERE conname = 'journal_entry_source_context_check' AND conrelid = 'ledger.journal_entry'::regclass`).Scan(&def, &unvalidated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "'collections'") || unvalidated != 0 {
		t.Errorf("source contexts: got %s with %d checks not validated, want collections among them, every check validated", def, unvalidated)
	}
}

// TestDisputeCaseOfThePreviousVersion (T86c, migration 000395): a case
// written as the previous version writes it, without items, commits and
// has its own line as its one item, on the line's account (the trigger
// dispute_case_default_item), so a release that still runs that version
// keeps opening cases and dunning holds their items.
func TestDisputeCaseOfThePreviousVersion(t *testing.T) {
	need(t)
	f := newFixture(t)
	c := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		entry, err := f.entry(tx, 2, "2026-02-10", line{account: f.bank, amount: 100}, line{account: f.expense, amount: -100})
		if err != nil {
			return err
		}
		return exec(tx, disputeCaseOf, f.tenant, c, entry)
	})
	var got string
	if err := testDB.QueryRow(context.Background(), `SELECT string_agg(i.item_no || ' ' || (i.account_id = $2)::text || ' ' || i.disputed_amount || ' '
			|| i.open, ', ' ORDER BY i.item_no) FROM collections.dispute_case_item i WHERE i.dispute_case_id = $1`, c, f.bank).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "1 true 100 true"; got != want {
		t.Errorf("the case's items: got %q, want %q", got, want)
	}
}
