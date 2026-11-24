package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesAvailabilityRules (S2j, migration 000430; ADR-0082 As built
// S2j): a sales order's plant is entered with it and never changed by the
// application; an order item's confirmed quantity is never entered with
// the item, is set only on an item of a material of a confirmed order
// that names a plant, at most the item's quantity when set (not when the
// quantity is lowered later: the change sets it again) and not below zero, is
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
		outbound_delivery_number, quantity, created_by, plant_id) VALUES ($1, gen_random_uuid(), $2, 1, gen_random_uuid(), 'OD1', $3::numeric, $1, gen_random_uuid())`
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
		{"more confirmed than ordered, as the owner", func(s scene) []stmt { return placed(s, true, step(promise, s.order, "6")) }, true,
			"sales_order_item_confirmed_by_check"},
		{"a confirmation cleared, as the owner", func(s scene) []stmt {
			return placed(s, true, step(promise, s.order, "1"), step(`UPDATE sales.sales_order_item SET confirmed_quantity = NULL WHERE sales_order_id = $1`, s.order))
		}, true, "sales_order_item_confirmed_by_check"},
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

// TestDeliveryAssignmentPlantBackfilled (migration 000430): an
// assignment written before the migration takes the plant its outbound
// delivery ships from; its down drops the column again.
func TestDeliveryAssignmentPlantBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	up := migrateBefore(t, conn, "000430_sales_availability")
	tenant, order, delivery, plant := id(), id(), id(), id()
	inTenant(t, conn, tenant, func(tx pgx.Tx) error {
		return all(tx,
			step(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
				net_amount, status, created_by, updated_by) VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100,
				'draft', $1, $1)`, tenant, order),
			step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
				net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 100, 1, 100, gen_random_uuid())`, tenant, order),
			step(`UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`, order),
			step(`INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id, sales_order_number,
				customer_id, customer_number, plant_id, storage_location_id, status, created_by, updated_by)
				VALUES ($1, $2, gen_random_uuid(), 2026, 'OD1', $3, 'SO1', gen_random_uuid(), '100001', $4, gen_random_uuid(), 'created', $1, $1)`,
				tenant, delivery, order, plant),
			step(`INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id, outbound_delivery_number,
				quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, $3, 'OD1', 2, $1)`, tenant, order, delivery))
	})
	if _, err := migrate(ctx, conn, []Migration{up}); err != nil {
		t.Fatal(err)
	}
	var got uuid.UUID
	if err := conn.QueryRow(ctx, `SELECT plant_id FROM sales.delivery_assignment WHERE outbound_delivery_id = $1`, delivery).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != plant {
		t.Errorf("the assignment's plant: got %s, want its delivery's %s", got, plant)
	}
	if _, err := conn.Exec(ctx, up.Down); err != nil {
		t.Fatalf("the down: %v", err)
	}
}

// TestDeliveryAssignmentPlantRequired (migrations 000458, 000459; ADR-0082
// As built S2j): the contract step makes an assignment's plant required.
// An assignment a server of the release before S2j wrote without one,
// after 000430's backfill, takes its outbound delivery's plant; the column
// is then NOT NULL, refusing an assignment without one; both downs make it
// nullable again.
func TestDeliveryAssignmentPlantRequired(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	required := migrateBefore(t, conn, "000458_delivery_assignment_plant_required")
	validated := migrationNamed(t, "000459_validate_delivery_assignment_plant")
	tenant, order, delivery, plant := id(), id(), id(), id()
	assignment := `INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id, outbound_delivery_number,
		quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, $3, 'OD1', $4::numeric, $1)`
	inTenant(t, conn, tenant, func(tx pgx.Tx) error {
		return all(tx,
			step(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
				net_amount, status, created_by, updated_by) VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100,
				'draft', $1, $1)`, tenant, order),
			step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
				net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 100, 1, 100, gen_random_uuid())`, tenant, order),
			step(`UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`, order),
			step(`INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id, sales_order_number,
				customer_id, customer_number, plant_id, storage_location_id, status, created_by, updated_by)
				VALUES ($1, $2, gen_random_uuid(), 2026, 'OD1', $3, 'SO1', gen_random_uuid(), '100001', $4, gen_random_uuid(), 'created', $1, $1)`,
				tenant, delivery, order, plant),
			step(assignment, tenant, order, delivery, "2"))
	})
	if _, err := migrate(ctx, conn, []Migration{required, validated}); err != nil {
		t.Fatal(err)
	}
	var got uuid.UUID
	if err := conn.QueryRow(ctx, `SELECT plant_id FROM sales.delivery_assignment WHERE outbound_delivery_id = $1`, delivery).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != plant {
		t.Errorf("the assignment's plant: got %s, want its delivery's %s", got, plant)
	}
	nullable := func() string {
		var n string
		if err := conn.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'sales' AND table_name = 'delivery_assignment' AND column_name = 'plant_id'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := nullable(); n != "NO" {
		t.Errorf("plant_id nullable: got %s, want NO", n)
	}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, assignment, tenant, order, delivery, "-2")
		return err
	})
	failsWith(t, err, `null value in column "plant_id" of relation "delivery_assignment"`)
	for _, m := range []Migration{validated, required} {
		if _, err := conn.Exec(ctx, m.Down); err != nil {
			t.Fatalf("%s's down: %v", m.Name, err)
		}
	}
	if n := nullable(); n != "YES" {
		t.Errorf("plant_id nullable after the downs: got %s, want YES", n)
	}
}
