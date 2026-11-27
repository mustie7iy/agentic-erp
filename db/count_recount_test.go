package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// earlier keeps the item's count as its earlier count no, as the
// application does: copied from the item.
func (s countScene) earlier(item, no int) stmt {
	return step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity, entry_quantity,
		entry_uom_code, counted_at, counted_by, counted_by_name, recounted_by, recounted_by_name)
		SELECT tenant_id, count_document_id, item_no, $3, counted_quantity, entry_quantity, entry_uom_code, counted_at, counted_by, counted_by_name,
			tenant_id, 'Ines'
		FROM inventory.count_item WHERE count_document_id = $1 AND item_no = $2`, s.count, item, no)
}

// cleared takes the item's count off and raises its recounts by one.
func (s countScene) cleared(item int) stmt {
	return step(`UPDATE inventory.count_item SET counted_quantity = NULL, entry_quantity = NULL, entry_uom_code = NULL, counted_at = NULL,
		counted_by = NULL, counted_by_name = NULL, recounts = recounts + 1 WHERE count_document_id = $1 AND item_no = $2`, s.count, item)
}

// TestRecountRules (S2d2, migration 000490; ADR-0082 As built S2d2): an
// earlier count is the open, unposted item's count as it is, numbered
// after its earlier ones, 1 to 98, and is never changed or deleted; a
// count is taken off an item only with its recounts one more and that
// earlier count kept; an item takes at most 98 recounts; a counted
// document goes back to counting only while an item of it is not counted.
func TestRecountRules(t *testing.T) {
	need(t)
	counted := func(s countScene) []stmt {
		return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.counted(1),
			step(`UPDATE inventory.count_document SET status = 'counted', version = 2 WHERE id = $1`, s.count)}
	}
	back := func(s countScene) stmt {
		return step(`UPDATE inventory.count_document SET status = 'counting', version = 3 WHERE id = $1`, s.count)
	}
	cases := []struct {
		name string
		run  func(s countScene) []stmt
		want string // empty: allowed
	}{
		{"a recount", func(s countScene) []stmt { return append(counted(s), s.earlier(1, 1), s.cleared(1), back(s)) }, ""},
		{"a recount counted again", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), s.cleared(1), back(s), s.counted(1), s.earlier(1, 2), s.cleared(1))
		}, ""},
		{"a count taken off without its earlier count", func(s countScene) []stmt { return append(counted(s), s.cleared(1)) }, "count_item_once"},
		{"a count taken off without its recounts", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), step(`UPDATE inventory.count_item SET counted_quantity = NULL, entry_quantity = NULL,
				entry_uom_code = NULL, counted_at = NULL, counted_by = NULL WHERE count_document_id = $1`, s.count))
		}, "count_item_once"},
		{"recounts raised alone", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), step(`UPDATE inventory.count_item SET recounts = 1 WHERE count_document_id = $1`, s.count))
		}, "count_item_once"},
		{"an earlier count numbered out of turn", func(s countScene) []stmt { return append(counted(s), s.earlier(1, 2)) }, "count_item_recount_check"},
		{"an earlier count unlike the item's", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 1, 8, 9, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_check"},
		{"an earlier count of an item not counted", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.earlier(1, 1)}
		}, "null value"},
		{"an earlier count of a closed item", func(s countScene) []stmt {
			return append(counted(s), step(`UPDATE inventory.count_item SET open = false WHERE count_document_id = $1`, s.count), s.earlier(1, 1))
		}, "count_item_recount_check"},
		{"an earlier count of no item", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 2, 1, 9, 9, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_item_fkey"},
		{"an earlier count numbered 0", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 0, 9, 9, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_count_no_check"},
		{"an earlier count numbered 99", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 99, 9, 9, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_count_no_check"},
		{"a negative earlier count", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 1, -1, 9, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_counted_quantity_check"},
		{"a negative earlier entry", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 1, 9, -1, 'EA', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_entry_quantity_check"},
		{"an earlier count in no unit", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by, recounted_by_name) VALUES ($1, $2, 1, 1, 9, 9, 'XX9', now(), $1, $1, 'Ines')`, s.tenant, s.count))
		}, "count_item_recount_entry_uom_code_fkey"},
		{"an earlier count changed", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), step(`UPDATE inventory.count_item_recount SET counted_quantity = 1`))
		}, "permission denied"},
		{"an earlier count deleted", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), step(`DELETE FROM inventory.count_item_recount`))
		}, "permission denied"},
		{"an earlier count's name unlike the item's", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, counted_by_name, recounted_by, recounted_by_name)
				SELECT tenant_id, count_document_id, item_no, 1, counted_quantity, entry_quantity, entry_uom_code, counted_at, counted_by, 'Someone else',
					tenant_id, 'Ines' FROM inventory.count_item WHERE count_document_id = $1`, s.count))
		}, "count_item_recount_check"},
		{"an earlier count without who sent it back", func(s countScene) []stmt {
			return append(counted(s), step(`INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity,
				entry_quantity, entry_uom_code, counted_at, counted_by, recounted_by)
				SELECT tenant_id, count_document_id, item_no, 1, counted_quantity, entry_quantity, entry_uom_code, counted_at, counted_by, tenant_id
				FROM inventory.count_item WHERE count_document_id = $1`, s.count))
		}, "null value"},
		{"a count's name changed", func(s countScene) []stmt {
			return append(counted(s), step(`UPDATE inventory.count_item SET counted_by_name = 'Someone else' WHERE count_document_id = $1`, s.count))
		}, "count_item_once"},
		{"a recount keeping the count's name", func(s countScene) []stmt {
			return append(counted(s), s.earlier(1, 1), step(`UPDATE inventory.count_item SET counted_quantity = NULL, entry_quantity = NULL,
				entry_uom_code = NULL, counted_at = NULL, counted_by = NULL, recounts = 1 WHERE count_document_id = $1`, s.count))
		}, "count_item_once"},
		{"a name without its count", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET counted_by_name = 'Wade' WHERE count_document_id = $1`, s.count)}
		}, "count_item_counted_by_name_check"},
		{"recounts 99", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(`INSERT INTO inventory.count_item (tenant_id, count_document_id, item_no, material_id,
				plant_id, storage_location_id, book_quantity, recounts) VALUES ($1, $2, 1, $3, $4, $5, 10, 99)`, s.tenant, s.count, s.material, s.plant,
				s.location)}
		}, "count_item_recounts_check"},
		{"a counted document back to counting", func(s countScene) []stmt { return append(counted(s), back(s)) }, "count_document_status_flow"},
		{"a counted document back once posted", func(s countScene) []stmt {
			return append(counted(s), step(`UPDATE inventory.count_document SET status = 'posted', currency = 'EUR', closed_at = now(), version = 3
				WHERE id = $1`, s.count), step(`UPDATE inventory.count_document SET status = 'counting', version = 4 WHERE id = $1`, s.count))
		}, "count_document_status_flow"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newCountScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, append(s.received()[:3], c.run(s)...)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			failsWith(t, err, c.want)
		})
	}
}

// TestRecountTriggersAsOwner (S2d2, migration 000490): past the grants,
// as the owner, an earlier count is neither changed nor deleted, a count
// is taken off an item only by a recount that keeps it, and a counted
// document does not go back to counting while every item is counted.
func TestRecountTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := newCountScene()
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"an earlier count changed": {step(`UPDATE inventory.count_item_recount SET counted_quantity = 1 WHERE count_document_id = $1`, s.count), "is insert-only"},
		"an earlier count deleted": {step(`DELETE FROM inventory.count_item_recount WHERE count_document_id = $1`, s.count), "is insert-only"},
		"a count taken off":        {s.cleared(1), "count_item_once"},
		"a counted document reopened": {step(`UPDATE inventory.count_document SET status = 'counting', version = 3 WHERE id = $1`, s.count),
			"count_document_status_flow"},
		"an earlier count of another": {s.earlier(1, 3), "count_item_recount_check"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			// Item 1 recounted once and counted again: its earlier count 1 kept.
			if err := all(tx, append(s.received()[:3], s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.counted(1),
				s.earlier(1, 1), s.cleared(1), s.counted(1),
				step(`UPDATE inventory.count_document SET status = 'counted', version = 2 WHERE id = $1`, s.count))...); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, c.st.sql, c.st.args...), c.want)
		})
	}
}

// TestRecountDownRefused (S2d2, migration 000490): the down refuses,
// rather than drops, earlier counts (06 §6.4: a down migration loses no
// data), reading every tenant's rows: a migrating role held to row-level
// security fails it rather than finding none.
func TestRecountDownRefused(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000490_count_recounts").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := newCountScene()
	if err := all(tx, append(append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.received()[:3]...),
		s.document(s.count, "PI1", "counted"), s.item(s.count, 1, s.material, "10"), s.counted(1), s.earlier(1, 1),
		step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`))...); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "earlier counts of count items exist")
}

// TestRecountDownRefusedNames (S2d2, migration 000490): with no earlier
// count, the down still refuses while a count names who entered it.
func TestRecountDownRefusedNames(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000490_count_recounts").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := newCountScene()
	if err := all(tx, append(append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.received()[:3]...),
		s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.counted(1),
		step(`SELECT set_config('app.tenant_id', '', true)`),
		// Other tests' earlier counts would meet the first guard; in this
		// transaction alone, rolled back, there are none. The replica role
		// passes the insert-only trigger without ALTER TABLE's exclusive
		// lock, which held up every other test reading the table.
		step(`SET LOCAL session_replication_role = replica`),
		step(`DELETE FROM inventory.count_item_recount`),
		step(`SET LOCAL session_replication_role = origin`))...); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "counts name who entered them")
}
