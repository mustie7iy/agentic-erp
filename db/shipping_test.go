package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestShippingPermissions (S2a, migration 000406; ADR-0083): the
// permissions of shipping are seeded and are no approval permissions.
func TestShippingPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'shipping.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "shipping.delivery.read shipping.delivery.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// shippingScene is a tenant with an outbound delivery of a company, by id.
type shippingScene struct{ tenant, company, delivery, document uuid.UUID }

const (
	shippingDelivery = `INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id, sales_order_number,
		customer_id, customer_number, plant_id, storage_location_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, 2026, $4, gen_random_uuid(), 'SO1', gen_random_uuid(), '100001', gen_random_uuid(), gen_random_uuid(), $5, $1, $1)`
	shippingItem = `INSERT INTO shipping.outbound_delivery_item (tenant_id, outbound_delivery_id, item_no, order_item_no, material_id, description,
		quantity, uom_code, picked_quantity, cost_amount) VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, $6::numeric, $7, $8::numeric, $9)`
	shippingIssue = `UPDATE shipping.outbound_delivery SET status = 'goods_issued', goods_issue_date = '2026-11-20', material_document_id = $2,
		material_document_number = 'MD1', version = version + 1 WHERE id = $1`
)

func (s shippingScene) header(number, status string) stmt {
	return step(shippingDelivery, s.tenant, s.delivery, s.company, number, status)
}

func (s shippingScene) item(no, orderItem int, description, qty, unit, picked string, cost any) stmt {
	return step(shippingItem, s.tenant, s.delivery, no, orderItem, description, qty, unit, picked, cost)
}

// made is the scene's delivery, created, with one item of 5 BOX.
func (s shippingScene) made() []stmt {
	return []stmt{s.header("OD1", "created"), s.item(1, 1, "Bracket", "5", "BOX", "0", nil)}
}

// TestShippingRules (S2a, migration 000406; ADR-0083 §2): a delivery has
// a number of 1 to 40 characters once per company and fiscal year and a
// known status, and names its goods issue's day and material document
// exactly when its goods issue is posted; its items, numbered from 1, take
// an order item once, a quantity above zero of a known unit, a picked
// quantity from zero to it, a description of 1 to 200 characters and a
// cost not below zero. While created the application picks and posts its
// goods issue, moving its version; then neither it nor its items change.
// Nothing is deleted, and only the granted columns change.
func TestShippingRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s shippingScene) []stmt
		want string // empty: allowed
	}{
		{"a delivery with an item", func(s shippingScene) []stmt { return s.made() }, ""},
		{"an empty number", func(s shippingScene) []stmt { return []stmt{s.header("", "created")} }, "outbound_delivery_number_check"},
		{"a number of 41 characters", func(s shippingScene) []stmt { return []stmt{s.header(strings.Repeat("O", 41), "created")} },
			"outbound_delivery_number_check"},
		{"a number twice in the company and year", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), step(shippingDelivery, s.tenant, id(), s.company, "OD1", "created")}
		}, "outbound_delivery_number_once"},
		{"the number in another company", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), step(shippingDelivery, s.tenant, id(), id(), "OD1", "created")}
		}, ""},
		{"an unknown status", func(s shippingScene) []stmt { return []stmt{s.header("OD1", "shipped")} }, "outbound_delivery_status_check"},
		{"goods issued without its day and document", func(s shippingScene) []stmt { return []stmt{s.header("OD1", "goods_issued")} },
			"outbound_delivery_goods_issue"},
		{"created with a goods issue's document", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET material_document_id = $2, material_document_number = 'MD1',
				version = version + 1 WHERE id = $1`, s.delivery, s.document))
		}, "outbound_delivery_goods_issue"},
		{"an item of nothing", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(1, 1, "Bracket", "0", "BOX", "0", nil)}
		}, "outbound_delivery_item_quantity_check"},
		{"an item 0", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(0, 1, "Bracket", "1", "BOX", "0", nil)}
		}, "outbound_delivery_item_no_check"},
		{"an order item 0", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(1, 0, "Bracket", "1", "BOX", "0", nil)}
		}, "outbound_delivery_item_order_item_no_check"},
		{"an order item twice", func(s shippingScene) []stmt {
			return append(s.made(), s.item(2, 1, "Bracket", "1", "BOX", "0", nil))
		}, "outbound_delivery_item_order_item_once"},
		{"an empty description", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(1, 1, "", "1", "BOX", "0", nil)}
		}, "outbound_delivery_item_description_check"},
		{"a description of 201 characters", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(1, 1, strings.Repeat("d", 201), "1", "BOX", "0", nil)}
		}, "outbound_delivery_item_description_check"},
		{"an unknown unit", func(s shippingScene) []stmt {
			return []stmt{s.header("OD1", "created"), s.item(1, 1, "Bracket", "1", "XX", "0", nil)}
		}, "outbound_delivery_item_uom_code_fkey"},
		{"an item of no delivery", func(s shippingScene) []stmt { return []stmt{s.item(1, 1, "Bracket", "1", "BOX", "0", nil)} },
			"outbound_delivery_item_delivery_fkey"},
		{"more picked than its quantity", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery_item SET picked_quantity = 5.000001`))
		}, "outbound_delivery_item_picked_check"},
		{"less than nothing picked", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery_item SET picked_quantity = -1`))
		}, "outbound_delivery_item_picked_check"},
		{"a cost below zero", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery_item SET cost_amount = -1`))
		}, "outbound_delivery_item_cost_check"},
		{"picked and issued", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery_item SET picked_quantity = 5, cost_amount = 1000`),
				step(shippingIssue, s.delivery, s.document))
		}, ""},
		{"a change without moving the version", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET updated_by = tenant_id`))
		}, "optimistic lock"},
		{"an issued delivery changed", func(s shippingScene) []stmt {
			return append(s.made(), step(shippingIssue, s.delivery, s.document), step(`UPDATE shipping.outbound_delivery SET version = version + 1`))
		}, "its goods issue is posted, so it changes no more"},
		{"an issued delivery's item picked", func(s shippingScene) []stmt {
			return append(s.made(), step(shippingIssue, s.delivery, s.document), step(`UPDATE shipping.outbound_delivery_item SET picked_quantity = 1`))
		}, "its goods issue is posted, so its items change no more"},
		{"a delivery's number changed", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET number = 'OD9', version = version + 1`))
		}, "permission denied"},
		{"an item's quantity changed", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery_item SET quantity = 2`))
		}, "permission denied"},
		{"a delivery deleted", func(s shippingScene) []stmt { return append(s.made(), step(`DELETE FROM shipping.outbound_delivery`)) },
			"permission denied"},
		{"an item deleted", func(s shippingScene) []stmt {
			return append(s.made(), step(`DELETE FROM shipping.outbound_delivery_item`))
		},
			"permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
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

// TestShippingTriggersAsOwner (S2a, migration 000406): past the grants,
// as the owner, a delivery and its items are not deleted, and once its
// goods issue is posted neither changes.
func TestShippingTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
	for name, c := range map[string]struct {
		issued bool
		st     stmt
		want   string
	}{
		"a delivery deleted":        {false, step(`DELETE FROM shipping.outbound_delivery WHERE id = $1`, s.delivery), "insert-only"},
		"an item deleted":           {false, step(`DELETE FROM shipping.outbound_delivery_item WHERE outbound_delivery_id = $1`, s.delivery), "insert-only"},
		"an issued delivery's text": {true, step(`UPDATE shipping.outbound_delivery SET number = 'OD9', version = version + 1 WHERE id = $1`, s.delivery), "outbound_delivery_issued_fixed"},
		"an issued item's quantity": {true, step(`UPDATE shipping.outbound_delivery_item SET quantity = 1 WHERE outbound_delivery_id = $1`, s.delivery),
			"outbound_delivery_item_issued_fixed"},
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
			setup := s.made()
			if c.issued {
				setup = append(setup, step(shippingIssue, s.delivery, s.document))
			}
			if err := all(tx, setup...); err != nil {
				t.Fatal(err)
			}
			err = exec(tx, c.st.sql, c.st.args...)
			if c.want == "insert-only" {
				failsWith(t, err, c.want)
				return
			}
			failsOn(t, err, c.want)
		})
	}
}

// TestDeliveryAssignmentRules (S2a, migration 000406; ADR-0083 §4): an
// assignment of an order item to an outbound delivery adds its signed
// quantity, never zero, to what the item has in delivery and moves the
// order's version; only a confirmed order takes one. What is in delivery
// is never below zero and, with what is delivered, never above what was
// ordered; an order holding some is not cancelled, and once all is given
// back it is. Assignments are insert-only, and the application does not
// set what is in delivery itself.
func TestDeliveryAssignmentRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 100, 1, 100, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	const assignment = `INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id,
		outbound_delivery_number, quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, gen_random_uuid(), 'OD1', $3::numeric, $4)`
	const delivered = `WITH d AS (INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		VALUES ($1, gen_random_uuid(), $2, 1, '2026-03-14', $1) RETURNING id)
		INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) SELECT $1, d.id, $2, 1, $3::numeric FROM d`
	const cancel = `UPDATE sales.sales_order SET status = 'cancelled', cancellation_reason = 'No longer needed', cancelled_at = now(),
		cancelled_by = tenant_id, cancelled_by_name = 'Sally', version = version + 1 WHERE id = $1`
	type scene struct{ tenant, actor, order uuid.UUID }
	confirmed := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(header, s.tenant, s.order, "SO1"), step(item, s.tenant, s.order), step(confirm, s.order)}, more...)
	}
	// shows fails unless the order's version, who changed it last and its item's quantity in delivery are these.
	shows := func(tx pgx.Tx, s scene, version int, by uuid.UUID, inDelivery string) error {
		var got string
		want := fmt.Sprintf("version %d by %s, in delivery %s", version, by, inDelivery)
		err := tx.QueryRow(context.Background(), `SELECT format('version %s by %s, in delivery %s', o.version, o.updated_by,
				(SELECT trim_scale(i.in_delivery_quantity)::text FROM sales.sales_order_item i WHERE i.sales_order_id = o.id))
			FROM sales.sales_order o WHERE o.id = $1`, s.order).Scan(&got)
		if err == nil && got != want {
			err = fmt.Errorf("the order: got %s, want %s", got, want)
		}
		return err
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an assignment adds to what is in delivery and moves the version", func(tx pgx.Tx, s scene) error {
			if err := all(tx, confirmed(s, step(assignment, s.tenant, s.order, "3", s.actor))...); err != nil {
				return err
			}
			return shows(tx, s, 3, s.actor, "3")
		}, ""},
		{"a negative assignment gives it back", func(tx pgx.Tx, s scene) error {
			if err := all(tx, confirmed(s, step(assignment, s.tenant, s.order, "3", s.actor), step(assignment, s.tenant, s.order, "-3", s.actor))...); err != nil {
				return err
			}
			return shows(tx, s, 4, s.actor, "0")
		}, ""},
		{"more in delivery than was ordered", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "5.000001", s.actor))...)
		}, "sales_order_item_in_delivery"},
		{"more given back than is in delivery", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "1", s.actor), step(assignment, s.tenant, s.order, "-2", s.actor))...)
		}, "sales_order_item_in_delivery"},
		{"delivered beyond what deliveries leave open", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "3", s.actor), step(delivered, s.tenant, s.order, "2.5"))...)
		}, "sales_order_item_in_delivery"},
		{"delivered within what deliveries leave open", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "3", s.actor), step(delivered, s.tenant, s.order, "2"))...)
		}, ""},
		{"an assignment of nothing", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "0", s.actor))...)
		}, "delivery_assignment_quantity_check"},
		{"an empty delivery number", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(`INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id,
				outbound_delivery_number, quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, gen_random_uuid(), '', 1, $1)`, s.tenant, s.order))...)
		}, "delivery_assignment_number_check"},
		{"an assignment of a draft order", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.order, "SO1"), step(item, s.tenant, s.order), step(assignment, s.tenant, s.order, "1", s.actor))
		}, "its sales order is not confirmed"},
		{"an assignment of no order item", func(tx pgx.Tx, s scene) error {
			return all(tx, step(assignment, s.tenant, s.order, "1", s.actor))
		}, "delivery_assignment_order_item_fkey"},
		{"an order holding some cancelled", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "1", s.actor), step(cancel, s.order))...)
		}, "an outbound delivery holds some of it"},
		{"an order given all back cancelled", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "1", s.actor), step(assignment, s.tenant, s.order, "-1", s.actor),
				step(cancel, s.order))...)
		}, ""},
		{"an assignment changed", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "1", s.actor), step(`UPDATE sales.delivery_assignment SET quantity = 2`))...)
		}, "permission denied"},
		{"an assignment deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(assignment, s.tenant, s.order, "1", s.actor), step(`DELETE FROM sales.delivery_assignment`))...)
		}, "permission denied"},
		{"what is in delivery set by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(`UPDATE sales.sales_order_item SET in_delivery_quantity = 1`))...)
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), order: id()}
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

// TestDeliveryAssignmentRulesAsOwner (S2a, migration 000406): past the
// grants, as the owner, an assignment stays insert-only, and with the
// triggers aside a removed item holds nothing in delivery
// (sales_order_item_removed_unshipped).
func TestDeliveryAssignmentRulesAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tenant, order := id(), id()
	setup := []stmt{
		step(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
			net_amount, status, created_by, updated_by) VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100,
			'draft', $1, $1)`, tenant, order),
		step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
			net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 100, 1, 100, gen_random_uuid())`, tenant, order),
		step(`UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`, order),
		step(`INSERT INTO sales.delivery_assignment (tenant_id, id, sales_order_id, order_item_no, outbound_delivery_id, outbound_delivery_number,
			quantity, created_by) VALUES ($1, gen_random_uuid(), $2, 1, gen_random_uuid(), 'OD1', 1, $1)`, tenant, order),
	}
	for name, c := range map[string]struct {
		replica bool
		st      stmt
		want    string
	}{
		"an assignment changed": {false, step(`UPDATE sales.delivery_assignment SET quantity = 2 WHERE sales_order_id = $1`, order), "insert-only"},
		"an assignment deleted": {false, step(`DELETE FROM sales.delivery_assignment WHERE sales_order_id = $1`, order), "insert-only"},
		"a held item removed": {true, step(`UPDATE sales.sales_order_item SET removed_in_change = 1 WHERE sales_order_id = $1`, order),
			"sales_order_item_removed_unshipped"},
		"an order holding some cancelled": {false, step(`UPDATE sales.sales_order SET status = 'cancelled', cancellation_reason = 'x', cancelled_at = now(),
			cancelled_by = tenant_id, cancelled_by_name = 'S', version = version + 1 WHERE id = $1`, order), "sales_order_cancelled_unshipped"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, setup...); err != nil {
				t.Fatal(err)
			}
			if c.replica {
				if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
					t.Fatal(err)
				}
			}
			err = exec(tx, c.st.sql, c.st.args...)
			if c.want == "insert-only" {
				failsWith(t, err, c.want)
				return
			}
			failsOn(t, err, c.want)
		})
	}
}
