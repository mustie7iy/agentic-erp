package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesAvailabilityRules (S2j, migration 000421; ADR-0082 As built
// S2j): a sales order's plant is entered with it and never changed by the
// application; an order item's confirmed quantity is never entered with
// the item, is set only on an item of a material of a confirmed order
// that names a plant, at most the item's quantity and not below zero, is
// never cleared, and stays at least what is delivered and in delivery of
// the item. The trigger's rules hold for the owner too.
func TestSalesAvailabilityRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by, plant_id)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1, $3)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, material_id, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id, confirmed_quantity) VALUES ($1, $2, 1, 'M1', $3, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid(), $4::numeric)`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`
	const promise = `UPDATE sales.sales_order_item SET confirmed_quantity = $2::numeric WHERE sales_order_id = $1`
	const assigned = `INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id,
		outbound_delivery_number, quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, gen_random_uuid(), 'OD1', $3::numeric, $1)`
	type scene struct {
		tenant, order, plant uuid.UUID
		material             *uuid.UUID
	}
	var none *string
	q := func(s string) *string { return &s }
	// placed is the order, with the scene's plant (uuid.Nil: none) and an item of its material (nil: none), confirmed when asked.
	placed := func(s scene, confirmed bool, more ...stmt) []stmt {
		var plant *uuid.UUID
		if s.plant != uuid.Nil {
			plant = &s.plant
		}
		out := []stmt{step(header, s.tenant, s.order, plant), step(item, s.tenant, s.order, s.material, none)}
		if confirmed {
			out = append(out, step(confirm, s.order))
		}
		return append(out, more...)
	}
	cases := []struct {
		name  string
		run   func(s scene) []stmt
		owner bool
		want  string // empty: allowed
	}{
		{"a confirmed item of a confirmed order with a plant", func(s scene) []stmt { return placed(s, true, step(promise, s.order, "3")) }, false, ""},
		{"confirmed in full, then less", func(s scene) []stmt {
			return placed(s, true, step(promise, s.order, "5"), step(promise, s.order, "2"))
		}, false, ""},
		{"confirmed with nothing", func(s scene) []stmt { return placed(s, true, step(promise, s.order, "0")) }, false, ""},
		{"an item entered confirmed", func(s scene) []stmt {
			return []stmt{step(header, s.tenant, s.order, s.plant), step(item, s.tenant, s.order, s.material, q("1"))}
		}, false, "sales_order_item_confirmed_by_check"},
		{"an item of a draft confirmed", func(s scene) []stmt { return placed(s, false, step(promise, s.order, "1")) }, false,
			"sales_order_item_confirmed_by_check"},
		{"an item of an order without a plant confirmed", func(s scene) []stmt {
			s.plant = uuid.Nil
			return placed(s, true, step(promise, s.order, "1"))
		}, false, "sales_order_item_confirmed_by_check"},
		{"an item without a material confirmed", func(s scene) []stmt {
			s.material = nil
			return []stmt{step(header, s.tenant, s.order, s.plant), step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description,
				quantity, uom_code, unit_price, price_unit, net_amount, account_id) VALUES ($1, $2, 1, 'Freight', 5, 'EA', 20, 1, 100, gen_random_uuid())`,
				s.tenant, s.order), step(confirm, s.order), step(promise, s.order, "1")}
		}, false, "sales_order_item_confirmed_by_check"},
		{"more confirmed than ordered", func(s scene) []stmt { return placed(s, true, step(promise, s.order, "5.000001")) }, false,
			"sales_order_item_confirmed_by_check"},
		{"a confirmation cleared", func(s scene) []stmt {
			return placed(s, true, step(promise, s.order, "1"), step(`UPDATE sales.sales_order_item SET confirmed_quantity = NULL WHERE sales_order_id = $1`, s.order))
		}, false, "sales_order_item_confirmed_by_check"},
		{"below zero", func(s scene) []stmt { return placed(s, true, step(promise, s.order, "-1")) }, false, "sales_order_item_confirmed"},
		{"below what is in delivery", func(s scene) []stmt {
			return placed(s, true, step(promise, s.order, "3"), step(assigned, s.tenant, s.order, "2"), step(promise, s.order, "1"))
		}, false, "sales_order_item_confirmed"},
		{"more in delivery than confirmed", func(s scene) []stmt {
			return placed(s, true, step(promise, s.order, "1"), step(assigned, s.tenant, s.order, "2"))
		}, false, "sales_order_item_confirmed"},
		{"the plant changed", func(s scene) []stmt {
			return placed(s, false, step(`UPDATE sales.sales_order SET plant_id = gen_random_uuid() WHERE id = $1`, s.order))
		}, false, "permission denied"},
		{"an item entered confirmed, as the owner", func(s scene) []stmt {
			return []stmt{step(header, s.tenant, s.order, s.plant), step(item, s.tenant, s.order, s.material, q("1"))}
		}, true, "sales_order_item_confirmed_by_check"},
		{"an item of a draft confirmed, as the owner", func(s scene) []stmt { return placed(s, false, step(promise, s.order, "1")) }, true,
			"sales_order_item_confirmed_by_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			material := id()
			s := scene{tenant: id(), order: id(), plant: id(), material: &material}
			do := func(tx pgx.Tx) error { return all(tx, c.run(s)...) }
			if c.owner {
				do = asOwner(do)
			}
			err := asApp(t, s.tenant, do)
			switch {
			case c.want == "permission denied":
				failsWith(t, err, c.want)
			case c.want != "":
				failsOn(t, err, c.want)
			case err != nil:
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}
