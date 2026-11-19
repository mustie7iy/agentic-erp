package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// countScene is inventoryScene with a count document of its location.
type countScene struct {
	inventoryScene
	count uuid.UUID
}

func newCountScene() countScene {
	return countScene{inventoryScene: inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()},
		count: id()}
}

const countDocumentInsert = `INSERT INTO inventory.count_document (tenant_id, id, legal_entity_id, fiscal_year, number, plant_id, storage_location_id,
		document_date, status, created_by, updated_by) VALUES ($1, $2, $3, 2026, $4, $5, $6, '2026-11-20', $7, $1, $1)`

func (s countScene) document(id uuid.UUID, number, status string) stmt {
	return step(countDocumentInsert, s.tenant, id, s.company, number, s.plant, s.location, status)
}

const countItemInsert = `INSERT INTO inventory.count_item (tenant_id, count_document_id, item_no, material_id, plant_id, storage_location_id,
		book_quantity) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`

func (s countScene) item(doc uuid.UUID, no int, material uuid.UUID, book string) stmt {
	return step(countItemInsert, s.tenant, doc, no, material, s.plant, s.location, book)
}

// counted sets the item's count whole.
func (s countScene) counted(no int) stmt {
	return step(`UPDATE inventory.count_item SET counted_quantity = 9, entry_quantity = 9, entry_uom_code = 'EA', counted_at = now(), counted_by = $1
		WHERE count_document_id = $2 AND item_no = $3`, s.tenant, s.count, no)
}

// TestCountRules (S2c, migration 000406; ADR-0082 As built S2c): a count
// document has a number of 1 to 40 characters once per company and fiscal
// year and a known status that only moves forward; its items are numbered
// 1 to 100, of its location, each material once, with a book quantity and
// a count of zero or more, a count entered whole and once, an item closed
// once; one open count per material and location; while one is open no
// movement moves the material there; documents and items are not deleted.
func TestCountRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s countScene) []stmt
		want string // empty: allowed
	}{
		{"a document and its item", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10")}
		}, ""},
		{"an empty number", func(s countScene) []stmt { return []stmt{s.document(s.count, "", "counting")} }, "count_document_number_check"},
		{"a number of 41 characters", func(s countScene) []stmt {
			return []stmt{s.document(s.count, strings.Repeat("P", 41), "counting")}
		}, "count_document_number_check"},
		{"a number twice", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.document(id(), "PI1", "counting")}
		}, "count_document_number_once"},
		{"an unknown status", func(s countScene) []stmt { return []stmt{s.document(s.count, "PI1", "open")} }, "count_document_status_check"},
		{"counting to posted", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(`UPDATE inventory.count_document SET status = 'posted' WHERE id = $1`, s.count)}
		}, "count_document_status_flow"},
		{"posted to counting", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "posted"), step(`UPDATE inventory.count_document SET status = 'counting' WHERE id = $1`, s.count)}
		}, "count_document_status_flow"},
		{"counting to counted", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(`UPDATE inventory.count_document SET status = 'counted', version = 2 WHERE id = $1`, s.count)}
		}, ""},
		{"a document's number changed", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(`UPDATE inventory.count_document SET number = 'PI9' WHERE id = $1`, s.count)}
		}, "permission denied"},
		{"a document deleted", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(`DELETE FROM inventory.count_document WHERE id = $1`, s.count)}
		}, "permission denied"},
		{"an item of no document", func(s countScene) []stmt { return []stmt{s.item(s.count, 1, s.material, "10")} }, "count_item_location_fkey"},
		{"an item of another location", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), step(countItemInsert, s.tenant, s.count, 1, s.material, s.plant, id(), "1")}
		}, "count_item_location_fkey"},
		{"item 0", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 0, s.material, "10")}
		}, "count_item_item_no_check"},
		{"item 101", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 101, s.material, "10")}
		}, "count_item_item_no_check"},
		{"a material twice", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.item(s.count, 2, s.material, "10")}
		}, "count_item_material_once"},
		{"a negative book quantity", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "-1")}
		}, "count_item_book_quantity_check"},
		{"a count in part", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET counted_quantity = 9 WHERE count_document_id = $1`, s.count)}
		}, "count_item_count_check"},
		{"a negative count", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET counted_quantity = -1, entry_quantity = 1, entry_uom_code = 'EA', counted_at = now(), counted_by = $1
					WHERE count_document_id = $2`, s.tenant, s.count)}
		}, "count_item_counted_quantity_check"},
		{"a count", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.counted(1)}
		}, ""},
		{"a count entered again", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.counted(1),
				step(`UPDATE inventory.count_item SET counted_quantity = 8 WHERE count_document_id = $1`, s.count)}
		}, "count_item_once"},
		{"an item reopened", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET open = false WHERE count_document_id = $1`, s.count),
				step(`UPDATE inventory.count_item SET open = true WHERE count_document_id = $1`, s.count)}
		}, "count_item_once"},
		{"a book quantity changed", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET book_quantity = 1 WHERE count_document_id = $1`, s.count)}
		}, "permission denied"},
		{"an item deleted", func(s countScene) []stmt {
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`DELETE FROM inventory.count_item WHERE count_document_id = $1`, s.count)}
		}, "permission denied"},
		{"two open counts of a material there", func(s countScene) []stmt {
			other := id()
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"), s.document(other, "PI2", "counting"),
				s.item(other, 1, s.material, "10")}
		}, "count_item_one_open"},
		{"a count of a material after its count closed", func(s countScene) []stmt {
			other := id()
			return []stmt{s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET open = false WHERE count_document_id = $1`, s.count), s.document(other, "PI2", "counting"),
				s.item(other, 1, s.material, "10")}
		}, ""},
		{"a movement of a material counted there", func(s countScene) []stmt {
			return append(s.received()[:3], s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				s.movement(s.doc, 1, "10", "10", "EA"))
		}, "count_posting_block"},
		{"a movement once the count closed", func(s countScene) []stmt {
			return append(s.received()[:3], s.document(s.count, "PI1", "counting"), s.item(s.count, 1, s.material, "10"),
				step(`UPDATE inventory.count_item SET open = false WHERE count_document_id = $1`, s.count), s.movement(s.doc, 1, "10", "10", "EA"))
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newCountScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			if strings.Contains(c.want, " ") {
				failsWith(t, err, c.want)
				return
			}
			failsOn(t, err, c.want)
		})
	}
}

// TestCountTriggersAsOwner (S2c, migration 000406): past the grants, as
// the owner, a document's status moves forward only, an item is counted
// and closed once and keeps what it counts, and a movement of a material
// counted there is refused.
func TestCountTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := newCountScene()
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"a document moved back":    {step(`UPDATE inventory.count_document SET status = 'counting' WHERE id = $1`, s.count), "count_document_status_flow"},
		"a count entered again":    {step(`UPDATE inventory.count_item SET counted_quantity = 1 WHERE count_document_id = $1`, s.count), "count_item_once"},
		"a book quantity changed":  {step(`UPDATE inventory.count_item SET book_quantity = 1 WHERE count_document_id = $1`, s.count), "count_item_once"},
		"an item moved":            {step(`UPDATE inventory.count_item SET material_id = $2 WHERE count_document_id = $1`, s.count, id()), "count_item_once"},
		"a movement counted there": {s.movement(s.doc, 1, "10", "10", "EA"), "count_posting_block"},
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
			if err := all(tx, append(s.received()[:3], s.document(s.count, "PI1", "counted"), s.item(s.count, 1, s.material, "10"), s.counted(1))...); err != nil {
				t.Fatal(err)
			}
			failsOn(t, exec(tx, c.st.sql, c.st.args...), c.want)
		})
	}
}
