package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLeadTimeRules (S2j2, migrations 000492 and 000493; ADR-0082 As
// built S2j2): a material plan's replenishment lead time is absent or 1
// to 999 days, and changed in place; an order item's part on the lead
// time and the day it ends are set together, only beside a confirmed
// quantity, more than zero and at most what is confirmed, and cleared
// together; what is confirmed is not lowered below it.
func TestLeadTimeRules(t *testing.T) {
	need(t)
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency,
		payment_term_code, net_amount, status, created_by, updated_by, plant_id)
		VALUES ($1, $2, 'SO7', gen_random_uuid(), gen_random_uuid(), '2099-01-01', 'EUR', 'NT30', 600, 'confirmed', $1, $1, gen_random_uuid())`
	const line = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, material_id, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id) VALUES ($1, $2, 1, 'M7', gen_random_uuid(), 'Hinge', 6, 'EA', 100, 1, 600, gen_random_uuid())`
	const set = `UPDATE sales.sales_order_item SET confirmed_quantity = $2::numeric, lead_time_quantity = $3::numeric, lead_time_date = $4::date
		WHERE sales_order_id = $1`
	none := (*string)(nil)
	q := func(s string) *string { return &s }
	day := q("2099-01-11")
	cases := []struct {
		name string
		run  func(s leadScene) []stmt
		want string // empty: allowed
	}{
		{"a plan with a lead time of a day", func(s leadScene) []stmt { return s.leadDays(q("1")) }, ""},
		{"a plan with a lead time of 999 days", func(s leadScene) []stmt { return s.leadDays(q("999")) }, ""},
		{"a plan without one", func(s leadScene) []stmt { return s.leadDays(none) }, ""},
		{"a lead time of no days", func(s leadScene) []stmt { return s.leadDays(q("0")) }, "material_plan_replenishment_lead_time_days_check"},
		{"a lead time of 1000 days", func(s leadScene) []stmt { return s.leadDays(q("1000")) }, "material_plan_replenishment_lead_time_days_check"},
		{"a lead time changed", func(s leadScene) []stmt {
			return append(s.leadDays(q("10")), step(`UPDATE mrp.material_plan SET replenishment_lead_time_days = 20, version = version + 1`))
		}, ""},
		{"part of the confirmation on the lead time", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", "2", day)) }, ""},
		{"all of it on the lead time", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", "5", day)) }, ""},
		{"more on the lead time than confirmed", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", "5.000001", day)) },
			"sales_order_item_lead_time"},
		{"nothing on the lead time", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", "0", day)) },
			"sales_order_item_lead_time"},
		{"a part without its day", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", "2", none)) },
			"sales_order_item_lead_time"},
		{"a day without its part", func(s leadScene) []stmt { return s.item(order, line, step(set, s.order, "5", nil, day)) },
			"sales_order_item_lead_time"},
		{"a part with nothing confirmed", func(s leadScene) []stmt {
			return s.item(order, line, step(`UPDATE sales.sales_order_item SET lead_time_quantity = 1, lead_time_date = '2099-01-11' WHERE sales_order_id = $1`, s.order))
		}, "sales_order_item_lead_time"},
		{"the confirmation lowered below it", func(s leadScene) []stmt {
			return s.item(order, line, step(set, s.order, "5", "4", day),
				step(`UPDATE sales.sales_order_item SET confirmed_quantity = 3 WHERE sales_order_id = $1`, s.order))
		}, "sales_order_item_lead_time"},
		{"both cleared", func(s leadScene) []stmt {
			return s.item(order, line, step(set, s.order, "5", "2", day), step(set, s.order, "5", nil, none))
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := leadScene{mrpScene: mrpScene{tenant: id(), plan: id(), plant: id(), material: id()}, order: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsOn(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// leadScene is a plan's scene with an order of its own.
type leadScene struct {
	mrpScene
	order uuid.UUID
}

// leadDays is the scene's plan with the lead time (nil: none).
func (s leadScene) leadDays(days *string) []stmt {
	return []stmt{s.planned("no_planning", "0"), step(`UPDATE mrp.material_plan SET replenishment_lead_time_days = $2::integer, version = version + 1 WHERE id = $1`, s.plan, days)}
}

// item is the scene's order, confirmed with a plant, and its one item,
// then more.
func (s leadScene) item(order, line string, more ...stmt) []stmt {
	return append([]stmt{step(order, s.tenant, s.order), step(line, s.tenant, s.order)}, more...)
}

// TestLeadTimeDownsRefused (batch 96S integration): the downs of 000493
// and 000492 refuse, rather than drop, an order item confirmed in part on
// the lead time and a plan's lead time (06 §6.4: a down migration loses
// no data), reading every tenant's rows.
func TestLeadTimeDownsRefused(t *testing.T) {
	need(t)
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency,
		payment_term_code, net_amount, status, created_by, updated_by, plant_id)
		VALUES ($1, $2, 'SO8', gen_random_uuid(), gen_random_uuid(), '2099-01-01', 'EUR', 'NT30', 600, 'confirmed', $1, $1, gen_random_uuid())`
	const line = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, material_id, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id) VALUES ($1, $2, 1, 'M8', gen_random_uuid(), 'Hinge', 6, 'EA', 100, 1, 600, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order_item SET confirmed_quantity = 5, lead_time_quantity = 2, lead_time_date = '2099-01-11' WHERE sales_order_id = $1`
	days := "10"
	for _, c := range []struct {
		name, migration, want string
		rows                  func(s leadScene) []stmt
	}{
		{"an item on the lead time", "000493_sales_lead_time", "sales order items are confirmed in part on the lead time",
			func(s leadScene) []stmt { return s.item(order, line, step(confirm, s.order)) }},
		{"a plan's lead time", "000492_mrp_lead_time", "material plans name replenishment lead times",
			func(s leadScene) []stmt { return s.leadDays(&days) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := leadScene{mrpScene: mrpScene{tenant: id(), plan: id(), plant: id(), material: id()}, order: id()}
			downRefused(t, migrationNamed(t, c.migration).Down, s.tenant, c.rows(s), c.want)
		})
	}
}
