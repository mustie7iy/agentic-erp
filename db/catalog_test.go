package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCatalogPermissions (T84a, migration 000276; ADR-0071 §5): the
// catalog permissions are seeded and are no approval permissions.
func TestCatalogPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'procurement.catalog.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "procurement.catalog.manage procurement.catalog.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestCatalogItemRules (T84a, migration 000276; ADR-0071 §1-§3): a
// catalog item is a supplier's in a company with a key, a description, a
// known unit and a positive price in a known currency; its key is the
// supplier's once among the company's active items; a contract is named
// with its item, which exists; an item is active or retired, and the
// application changes its status alone and never deletes it.
func TestCatalogItemRules(t *testing.T) {
	need(t)
	const item = `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code, currency,
		unit_price, price_unit, contract_id, contract_item_no, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $1, $1)`
	const contract = `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from, valid_to,
		target_value, status, created_by, updated_by) VALUES ($1, $2, 'C1', $3, $4, 'EUR', '2026-01-01', '2026-12-31', 100, 'active', $1, $1)`
	const contractItem = `INSERT INTO procurement.contract_item (tenant_id, contract_id, item_no, material_key, description, unit_price, uom_code, account_id)
		VALUES ($1, $2, 1, 'A', 'Paper', 500, 'EA', gen_random_uuid())`
	type scene struct{ tenant, item, entity, supplier, contract uuid.UUID }
	// loaded is an item of the scene's supplier: its id, key, description,
	// unit, currency, price, price unit and status.
	loaded := func(s scene, id uuid.UUID, key, description, unit, currency string, price, priceUnit int, status string) stmt {
		return step(item, s.tenant, id, s.entity, s.supplier, key, description, unit, currency, price, priceUnit, nil, nil, status)
	}
	ok := func(s scene) stmt {
		return loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 45000, 1, "active")
	}
	withContract := func(s scene, contractID *uuid.UUID, itemNo *int) stmt {
		return step(item, s.tenant, s.item, s.entity, s.supplier, "PAPER", "Paper", "EA", "EUR", 450, 1, contractID, itemNo, "active")
	}
	one, nine := 1, 9
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an active item", func(tx pgx.Tx, s scene) error { return all(tx, ok(s)) }, ""},
		{"a retired item", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 45000, 1, "retired"))
		}, ""},
		{"an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 45000, 1, "draft"))
		}, "catalog_item_status_check"},
		{"no item key", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "", "Office chair", "EA", "EUR", 45000, 1, "active"))
		}, "catalog_item_item_key_check"},
		{"an item key of 41 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, strings.Repeat("K", 41), "Office chair", "EA", "EUR", 45000, 1, "active"))
		}, "catalog_item_item_key_check"},
		{"an item key with white space around it", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, " CHAIR-01", "Office chair", "EA", "EUR", 45000, 1, "active"))
		}, "catalog_item_item_key_check"},
		{"a description of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "  ", "EA", "EUR", 45000, 1, "active"))
		}, "catalog_item_description_check"},
		{"a description of 201 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", strings.Repeat("d", 201), "EA", "EUR", 45000, 1, "active"))
		}, "catalog_item_description_check"},
		{"an unknown unit", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "ZZZ", "EUR", 45000, 1, "active"))
		}, "catalog_item_uom_code_fkey"},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "XXA", 45000, 1, "active"))
		}, "catalog_item_currency_fkey"},
		{"a price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 0, 1, "active"))
		}, "catalog_item_unit_price_check"},
		{"a price unit of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 45000, 0, "active"))
		}, "catalog_item_price_unit_check"},
		{"the supplier's key twice among the company's active items", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), loaded(s, id(), "CHAIR-01", "Another chair", "EA", "EUR", 100, 1, "active"))
		}, "catalog_item_active_key"},
		{"the key again after the first item is retired", func(tx pgx.Tx, s scene) error {
			return all(tx, loaded(s, s.item, "CHAIR-01", "Office chair", "EA", "EUR", 45000, 1, "retired"),
				loaded(s, id(), "CHAIR-01", "Office chair", "EA", "EUR", 47000, 1, "active"))
		}, ""},
		{"the key of another supplier's item", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(item, s.tenant, id(), s.entity, id(), "CHAIR-01", "Chair", "EA", "EUR", 100, 1, nil, nil, "active"))
		}, ""},
		{"the supplier's key in another company", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(item, s.tenant, id(), id(), s.supplier, "CHAIR-01", "Chair", "EA", "EUR", 100, 1, nil, nil, "active"))
		}, ""},
		{"a contract item that exists", func(tx pgx.Tx, s scene) error {
			return all(tx, step(contract, s.tenant, s.contract, s.entity, s.supplier), step(contractItem, s.tenant, s.contract), withContract(s, &s.contract, &one))
		}, ""},
		{"an item the contract does not have", func(tx pgx.Tx, s scene) error {
			return all(tx, step(contract, s.tenant, s.contract, s.entity, s.supplier), step(contractItem, s.tenant, s.contract), withContract(s, &s.contract, &nine))
		}, "catalog_item_contract_item_fkey"},
		{"a contract without its item", func(tx pgx.Tx, s scene) error {
			return all(tx, step(contract, s.tenant, s.contract, s.entity, s.supplier), step(contractItem, s.tenant, s.contract), withContract(s, &s.contract, nil))
		}, "catalog_item_contract_item_both"},
		{"a contract item without its contract", func(tx pgx.Tx, s scene) error {
			return all(tx, withContract(s, nil, &one))
		}, "catalog_item_contract_item_both"},
		{"an item retired", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(`UPDATE procurement.catalog_item SET status = 'retired', version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.item))
		}, ""},
		{"an item's price changed", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(`UPDATE procurement.catalog_item SET unit_price = 1 WHERE id = $1`, s.item))
		}, "permission denied"},
		{"an item's key changed", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(`UPDATE procurement.catalog_item SET item_key = 'OTHER' WHERE id = $1`, s.item))
		}, "permission denied"},
		{"an item's supplier changed", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(`UPDATE procurement.catalog_item SET supplier_id = gen_random_uuid() WHERE id = $1`, s.item))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, ok(s), step(`DELETE FROM procurement.catalog_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), item: id(), entity: id(), supplier: id(), contract: id()}
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

// TestCatalogSearchText (T84a, migration 000276): what the search reads
// is the item's key and description in lower case, kept by the database.
func TestCatalogSearchText(t *testing.T) {
	need(t)
	tenant, item := id(), id()
	var got string
	mustApp(t, tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code, currency,
			unit_price, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'CHAIR-01', 'Office Chair', 'EA', 'EUR', 1, $1, $1)`, tenant, item); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT search_text FROM procurement.catalog_item WHERE id = $1`, item).Scan(&got)
	})
	if want := "chair-01 office chair"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRequisitionItemCatalogItem (T84b, migrations 000277-000278;
// ADR-0071 §6): a requisition item names no catalog item or one that
// exists in its tenant, and the foreign key is validated; an update that
// changes what the item took from the catalog without naming the column,
// as the previous release's does during a rollout, ends the link.
func TestRequisitionItemCatalogItem(t *testing.T) {
	need(t)
	const requisition = `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, requisition_date,
		status, created_by, updated_by) VALUES ($1, $2, 'PR1', $3, $1, 'EUR', '2026-06-15', 'draft', $1, $1)`
	const catalogItem = `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code, currency,
		unit_price, created_by, updated_by) VALUES ($1, $2, $3, gen_random_uuid(), 'CHAIR-01', 'Office chair', 'EA', 'EUR', 45000, $1, $1)`
	const item = `INSERT INTO procurement.requisition_item (tenant_id, requisition_id, item_no, description, quantity, uom_code, unit_price, account_id,
		catalog_item_id) VALUES ($1, $2, 1, 'Office chair', 2, 'EA', 45000, gen_random_uuid(), $3)`
	type scene struct{ tenant, requisition, entity, catalogItem uuid.UUID }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an item the requester described", func(tx pgx.Tx, s scene) error {
			return all(tx, step(requisition, s.tenant, s.requisition, s.entity), step(item, s.tenant, s.requisition, nil))
		}, ""},
		{"an item from a catalog item", func(tx pgx.Tx, s scene) error {
			return all(tx, step(requisition, s.tenant, s.requisition, s.entity), step(catalogItem, s.tenant, s.catalogItem, s.entity),
				step(item, s.tenant, s.requisition, s.catalogItem))
		}, ""},
		{"an item from a catalog item that does not exist", func(tx pgx.Tx, s scene) error {
			return all(tx, step(requisition, s.tenant, s.requisition, s.entity), step(item, s.tenant, s.requisition, s.catalogItem))
		}, "requisition_item_catalog_item_fkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), requisition: id(), entity: id(), catalogItem: id()}
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
	// The previous release's upsert rewrites an item without naming the
	// column: what it changes of what the item took ends the link; a
	// rewrite with the same values, or of the quantity alone, keeps it.
	const old = `UPDATE procurement.requisition_item SET description = $3, uom_code = $4, unit_price = $5, price_unit = $6, quantity = 5
		WHERE tenant_id = $1 AND requisition_id = $2 AND item_no = 1`
	for _, c := range []struct {
		name, description, unit string
		price, priceUnit        int
		kept                    bool
	}{
		{"the same values and another quantity", "Office chair", "EA", 45000, 1, true},
		{"another description", "Paper", "EA", 45000, 1, false},
		{"another unit", "Office chair", "KG", 45000, 1, false},
		{"another price", "Office chair", "EA", 100, 1, false},
		{"another price unit", "Office chair", "EA", 45000, 2, false},
	} {
		t.Run("rewritten by the previous release with "+c.name, func(t *testing.T) {
			s := scene{tenant: id(), requisition: id(), entity: id(), catalogItem: id()}
			var link *uuid.UUID
			mustApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := all(tx, step(requisition, s.tenant, s.requisition, s.entity), step(catalogItem, s.tenant, s.catalogItem, s.entity),
					step(item, s.tenant, s.requisition, s.catalogItem), step(old, s.tenant, s.requisition, c.description, c.unit, c.price, c.priceUnit)); err != nil {
					return err
				}
				return tx.QueryRow(context.Background(), `SELECT catalog_item_id FROM procurement.requisition_item WHERE requisition_id = $1`, s.requisition).Scan(&link)
			})
			if kept := link != nil; kept != c.kept {
				t.Errorf("the item's catalog item kept: got %v, want %v", kept, c.kept)
			}
		})
	}
	var validated bool
	if err := testDB.QueryRow(context.Background(), `SELECT convalidated FROM pg_constraint WHERE conname = 'requisition_item_catalog_item_fkey'`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Error("requisition_item_catalog_item_fkey: got not validated, want validated (000278)")
	}
}
