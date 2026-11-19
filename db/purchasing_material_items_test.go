package db

import (
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
