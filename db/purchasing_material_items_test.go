package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCatalogItemMaterialRule (S1f2, migration 000397): a catalog item's
// material is set once, when it is loaded or by a mapping, and never
// changed or cleared after, for the tables' owner too.
func TestCatalogItemMaterialRule(t *testing.T) {
	need(t)
	const load = `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code, currency,
		unit_price, created_by, updated_by, material_id) VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), '100000001', 'Bracket', 'EA', 'EUR',
		250, $1, $1, $3)`
	const set = `UPDATE procurement.catalog_item SET material_id = $2, version = version + 1 WHERE id = $1`
	owner := step(`SET LOCAL ROLE erp_owner`)
	material := id()
	cases := []struct {
		name string
		run  func(tenant, item uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"loaded with a material", func(tenant, item uuid.UUID) []stmt { return []stmt{step(load, tenant, item, material)} }, ""},
		{"mapped once", func(tenant, item uuid.UUID) []stmt {
			return []stmt{step(load, tenant, item, nil), step(set, item, material)}
		}, ""},
		{"set again to the same", func(tenant, item uuid.UUID) []stmt {
			return []stmt{step(load, tenant, item, material), step(set, item, material)}
		}, ""},
		{"changed", func(tenant, item uuid.UUID) []stmt {
			return []stmt{step(load, tenant, item, material), step(set, item, id())}
		}, "keeps its material"},
		{"cleared", func(tenant, item uuid.UUID) []stmt {
			return []stmt{step(load, tenant, item, material), step(set, item, nil)}
		}, "keeps its material"},
		{"changed by the tables' owner", func(tenant, item uuid.UUID) []stmt {
			return []stmt{step(load, tenant, item, material), owner, step(set, item, id())}
		}, "keeps its material"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, item := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, c.run(tenant, item)...) })
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

// TestRequisitionItemMaterialNumber (S1f2, migration 000397): a
// requisition item keeps a number only beside the material it names, of
// 1 to 40 characters; an item may name a material without one (entered
// before S1f2).
func TestRequisitionItemMaterialNumber(t *testing.T) {
	need(t)
	const requisition = `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, requisition_date,
		status, created_by, updated_by) VALUES ($1, $2, 'PR1', gen_random_uuid(), $1, 'EUR', '2026-06-15', 'draft', $1, $1)`
	const item = `INSERT INTO procurement.requisition_item (tenant_id, requisition_id, item_no, description, quantity, uom_code, unit_price, account_id,
		material_id, material_number) VALUES ($1, $2, 1, 'Bracket', 2, 'EA', 100, gen_random_uuid(), $3, $4)`
	for _, c := range []struct {
		name     string
		material any
		number   any
		want     string
	}{
		{"a material with its number", id(), "100000001", ""},
		{"a material without a number", id(), nil, ""},
		{"neither", nil, nil, ""},
		{"a number without a material", nil, "100000001", "requisition_item_material_number"},
		{"an empty number", id(), "", "requisition_item_material_number"},
		{"a number of 41 characters", id(), strings.Repeat("9", 41), "requisition_item_material_number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant, req := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(requisition, tenant, req), step(item, tenant, req, c.material, c.number))
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
