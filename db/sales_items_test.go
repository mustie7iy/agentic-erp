package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesOrderItemRules (T78e, migration 000364; ADR-0067, As built
// T78e): an item is added to an order before any change of it, or with
// the order's next change kept, numbered after its highest, never to a
// cancelled order; an item is removed only with the order's next change
// kept, which it names, with nothing delivered, and then changes no more
// and takes no delivery; the payment term changes with a kept change
// while no billing document of the order stands; a change keeps both
// payment terms or neither; the rules hold for the tables' owner too.
func TestSalesOrderItemRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 200, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`
	const kept = `INSERT INTO sales.sales_order_change (tenant_id, id, sales_order_id, change_no, order_version, net_amount_before, net_amount_after,
		credit_block_before, credit_block_after, payment_term_code_before, payment_term_code_after, items, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, 3, 200, 200, 'none', 'none', $4, $5, '[]', $1)`
	const changed = `UPDATE sales.sales_order SET changes = $2, version = version + 1 WHERE id = $1`
	const termed = `UPDATE sales.sales_order SET changes = $2, payment_term_code = 'NT14', version = version + 1 WHERE id = $1`
	const remove = `UPDATE sales.sales_order_item SET removed_in_change = $2 WHERE sales_order_id = $1 AND item_no = 2`
	const cancel = `UPDATE sales.sales_order SET status = 'cancelled', cancellation_reason = 'Withdrawn', cancelled_at = now(), cancelled_by = tenant_id,
		cancelled_by_name = 'Sally Sales', version = version + 1 WHERE id = $1`
	const delivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		VALUES ($1, $3, $2, 1, '2026-03-14', $1)`
	const deliveryItem = `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) VALUES ($1, $3, $2, $4, 1)`
	const billing = `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
		currency, net_amount, status, created_by, updated_by)
		SELECT tenant_id, $2, 'BD1', legal_entity_id, customer_id, id, '2026-03-15', currency, 20, 'created', tenant_id, tenant_id
		FROM sales.sales_order WHERE id = $1`
	type scene struct{ tenant, order, doc uuid.UUID }
	var none *string
	term := func(s string) *string { return &s }
	confirmed := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(header, s.tenant, s.order), step(item, s.tenant, s.order, 1), step(item, s.tenant, s.order, 2),
			step(confirm, s.order)}, more...)
	}
	change := func(s scene, no int) stmt { return step(kept, s.tenant, s.order, no, none, none) }
	delivered := func(s scene, no int) []stmt {
		return []stmt{step(delivery, s.tenant, s.order, s.doc), step(deliveryItem, s.tenant, s.order, s.doc, no)}
	}
	owner := step(`SET LOCAL ROLE erp_owner`)
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"an item added with the change kept", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(item, s.tenant, s.order, 3), step(changed, s.order, 1))
		}, ""},
		{"an item added after a change without its own", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(changed, s.order, 1), step(item, s.tenant, s.order, 3))
		}, "an item is added to an order that is not cancelled with a kept change"},
		{"an item added after a change without its own, by the tables' owner", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(changed, s.order, 1), owner, step(item, s.tenant, s.order, 3))
		}, "an item is added to an order that is not cancelled with a kept change"},
		{"an item added below the highest", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(item, s.tenant, s.order, 2))
		}, "numbered after the order's highest"},
		{"an item added to a cancelled order", func(s scene) []stmt {
			return confirmed(s, step(cancel, s.order), step(item, s.tenant, s.order, 3))
		}, "an item is added to an order that is not cancelled with a kept change"},
		{"an item removed with the change kept", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(remove, s.order, 1), step(changed, s.order, 1))
		}, ""},
		{"an item removed without a change kept", func(s scene) []stmt { return confirmed(s, step(remove, s.order, 1)) },
			"an item is removed with the order's next change kept"},
		{"an item removed naming another change", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(remove, s.order, 2))
		}, "an item is removed with the order's next change kept"},
		{"an item removed without a change kept, by the tables' owner", func(s scene) []stmt {
			return confirmed(s, owner, step(remove, s.order, 1))
		}, "an item is removed with the order's next change kept"},
		{"a removed item changed", func(s scene) []stmt {
			return confirmed(s, change(s, 1), step(remove, s.order, 1), step(changed, s.order, 1),
				step(`UPDATE sales.sales_order_item SET quantity = 6 WHERE item_no = 2`))
		}, "it is removed, so it changes no more"},
		{"a removed item delivered", func(s scene) []stmt {
			return confirmed(s, append([]stmt{change(s, 1), step(remove, s.order, 1), step(changed, s.order, 1)}, delivered(s, 2)...)...)
		}, "sales_order_item_removed_undelivered"},
		{"a delivered item removed", func(s scene) []stmt {
			return confirmed(s, append(delivered(s, 2), change(s, 1), step(remove, s.order, 1))...)
		}, "sales_order_item_removed_undelivered"},
		{"the payment term changed with the change kept", func(s scene) []stmt {
			return confirmed(s, step(kept, s.tenant, s.order, 1, term("NT30"), term("NT14")), step(termed, s.order, 1))
		}, ""},
		{"the payment term changed without a change kept", func(s scene) []stmt {
			return confirmed(s, step(`UPDATE sales.sales_order SET payment_term_code = 'NT14' WHERE id = $1`, s.order))
		}, "an order not cancelled is changed by a kept change"},
		{"the payment term changed with the confirmation", func(s scene) []stmt {
			return []stmt{step(header, s.tenant, s.order), step(item, s.tenant, s.order, 1),
				step(`UPDATE sales.sales_order SET status = 'confirmed', payment_term_code = 'NT14', version = version + 1 WHERE id = $1`, s.order)}
		}, "its payment term changes with a kept change"},
		{"the payment term changed once billed", func(s scene) []stmt {
			return confirmed(s, append(delivered(s, 1), step(billing, s.order, id()), step(kept, s.tenant, s.order, 1, term("NT30"), term("NT14")),
				step(termed, s.order, 1))...)
		}, "it is billed, so its payment term stays"},
		{"the payment term changed once billed, by the tables' owner", func(s scene) []stmt {
			return confirmed(s, append(delivered(s, 1), step(billing, s.order, id()), owner, step(kept, s.tenant, s.order, 1, term("NT30"), term("NT14")),
				step(termed, s.order, 1))...)
		}, "it is billed, so its payment term stays"},
		{"a change with the payment term before alone", func(s scene) []stmt {
			return confirmed(s, step(kept, s.tenant, s.order, 1, term("NT30"), none))
		}, "sales_order_change_payment_term"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), order: id(), doc: id()}
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

// TestSalesOrderItemRevert (T78e, migration 000364's down): the revert
// is refused, before it changes anything, while an order has an item
// removed by a change.
func TestSalesOrderItemRevert(t *testing.T) {
	need(t)
	down := ""
	for _, m := range Migrations() {
		if strings.HasSuffix(m.Name, "_sales_order_items_added_removed") {
			down = m.Down
		}
	}
	if down == "" {
		t.Fatal("got no migration named …_sales_order_items_added_removed in db/migrations, want the one the test is about")
	}
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, order := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
		step(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency, payment_term_code,
			net_amount, status, created_by, updated_by)
			VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`, tenant, order),
		step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
			net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid()), ($1, $2, 2, 'Bolt', 5, 'EA', 20, 1, 100, gen_random_uuid())`,
			tenant, order),
		step(`INSERT INTO sales.sales_order_change (tenant_id, id, sales_order_id, change_no, order_version, net_amount_before, net_amount_after,
			credit_block_before, credit_block_after, items, created_by) VALUES ($1, gen_random_uuid(), $2, 1, 2, 200, 100, 'none', 'none', '[]', $1)`, tenant, order),
		step(`UPDATE sales.sales_order_item SET removed_in_change = 1 WHERE sales_order_id = $1 AND item_no = 2`, order)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would drop them")
}
