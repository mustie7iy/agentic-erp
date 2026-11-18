package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesOrderChangeRules (T78d, migration 000355; ADR-0067, As built
// T78d): an order that is not cancelled is changed only together with a
// kept change numbered as its next, its status kept and, confirmed,
// possibly blocked for credit again; an order of which nothing is
// delivered is cancelled once, naming its reason, who and when; a
// cancelled order changes no more; an item's quantity and price change
// only while nothing of it is billed and the order is not cancelled, the
// rules holding for the tables' owner too; a change is written once,
// numbered once within its order, and never changed.
func TestSalesOrderChangeRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`
	const kept = `INSERT INTO sales.sales_order_change (tenant_id, id, sales_order_id, change_no, order_version, reason, net_amount_before, net_amount_after,
		credit_block_before, credit_block_after, items, created_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, $7, $8, $9, $10, $1)`
	const changed = `UPDATE sales.sales_order SET changes = $2, net_amount = $3, credit_block = $4, version = version + 1 WHERE id = $1`
	const cancel = `UPDATE sales.sales_order SET status = 'cancelled', cancellation_reason = $2, cancelled_at = now(), cancelled_by = tenant_id,
		cancelled_by_name = 'Sally Sales',
		version = version + 1 WHERE id = $1`
	const delivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		VALUES ($1, $3, $2, 1, '2026-03-14', $1)`
	const deliveryItem = `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity)
		VALUES ($1, $3, $2, 1, 1)`
	const billing = `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
		currency, net_amount, status, created_by, updated_by)
		SELECT tenant_id, $2, 'BD1', legal_entity_id, customer_id, id, '2026-03-15', currency, 20, 'created', tenant_id, tenant_id
		FROM sales.sales_order WHERE id = $1`
	const billingItem = `INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
		VALUES ($1, $3, 1, $2, 1, 1, 20)`
	const price = `UPDATE sales.sales_order_item SET quantity = 6, net_amount = 120 WHERE sales_order_id = $1`
	// created is header with one more column set to the value.
	created := func(column, value string) string {
		return strings.Replace(strings.Replace(header, "status, created_by", "status, "+column+", created_by", 1), "'draft', $1", "'draft', "+value+", $1", 1)
	}
	type scene struct{ tenant, order, doc uuid.UUID }
	draft := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(header, s.tenant, s.order), step(item, s.tenant, s.order)}, more...)
	}
	confirmed := func(s scene, more ...stmt) []stmt {
		return draft(s, append([]stmt{step(confirm, s.order)}, more...)...)
	}
	change := func(s scene, no, version int, before, after string) stmt {
		return step(kept, s.tenant, s.order, no, version, nil, 100, 120, before, after, "[]")
	}
	delivered := func(s scene) []stmt {
		return []stmt{step(delivery, s.tenant, s.order, s.doc), step(deliveryItem, s.tenant, s.order, s.doc)}
	}
	billed := func(s scene) []stmt {
		return append(delivered(s), step(billing, s.order, s.doc), step(billingItem, s.tenant, s.order, s.doc))
	}
	owner := step(`SET LOCAL ROLE erp_owner`)
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a draft changed with its change kept", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "none"), step(changed, s.order, 1, 120, "none"), step(price, s.order))...)
		}, ""},
		{"a confirmed order changed and blocked again", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, change(s, 1, 3, "none", "blocked"), step(changed, s.order, 1, 120, "blocked"))...)
		}, ""},
		{"a changed order cancelled", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, change(s, 1, 3, "none", "none"), step(changed, s.order, 1, 120, "none"), step(cancel, s.order, "Withdrawn"))...)
		}, ""},
		{"changed without a change kept", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(changed, s.order, 1, 120, "none"))...)
		}, "only a draft changes"},
		{"changed past its next change", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, change(s, 2, 3, "none", "none"), step(changed, s.order, 2, 120, "none"))...)
		}, "only a draft changes"},
		{"changed without moving the change count", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, step(`UPDATE sales.sales_order SET net_amount = 120 WHERE id = $1`, s.order))...)
		}, "only a draft changes"},
		{"a draft blocked by a change", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "blocked"), step(changed, s.order, 1, 120, "blocked"))...)
		}, "only a draft changes"},
		{"a block lifted by a change", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, change(s, 1, 3, "none", "none"), step(changed, s.order, 1, 120, "blocked"),
				change(s, 2, 4, "blocked", "none"), step(changed, s.order, 2, 120, "none"))...)
		}, "only a draft changes"},
		{"a draft cancelled", func(tx pgx.Tx, s scene) error { return all(tx, draft(s, step(cancel, s.order, "Entered twice"))...) }, ""},
		{"a delivered order cancelled", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, append(delivered(s), step(cancel, s.order, "Too late"))...)...)
		}, "something of it is delivered, so it is not cancelled"},
		{"cancelled twice", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, "Once"), step(cancel, s.order, "Twice"))...)
		}, "only a draft changes"},
		{"a cancelled order changed", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, "Once"), change(s, 1, 3, "none", "none"), step(changed, s.order, 1, 120, "none"))...)
		}, "only a draft changes"},
		{"a cancelled order confirmed", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, "Once"), step(confirm, s.order))...)
		}, "only a draft changes"},
		{"a cancelled order takes a delivery", func(tx pgx.Tx, s scene) error {
			return all(tx, append(draft(s, step(cancel, s.order, "Once")), delivered(s)...)...)
		}, "its sales order is not confirmed"},
		{"cancelled without a reason", func(tx pgx.Tx, s scene) error { return all(tx, draft(s, step(cancel, s.order, nil))...) }, "sales_order_cancelled"},
		{"cancelled without the name of who cancelled it", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(strings.Replace(cancel, "'Sally Sales'", "NULL", 1), s.order, "Once"))...)
		}, "sales_order_cancelled"},
		{"cancelled for a reason of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, " "))...)
		}, "sales_order_cancellation_reason_check"},
		{"a reason of 2001 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, strings.Repeat("x", 2001)))...)
		}, "sales_order_cancellation_reason_check"},
		{"a reason on an order not cancelled", func(tx pgx.Tx, s scene) error {
			return all(tx, step(created("cancellation_reason", "'No'"), s.tenant, s.order))
		}, "sales_order_cancelled"},
		{"created cancelled without a reason", func(tx pgx.Tx, s scene) error {
			return all(tx, step(strings.Replace(created("cancelled_at, cancelled_by", "now(), $1"), "'draft'", "'cancelled'", 1), s.tenant, s.order))
		}, "sales_order_cancelled"},
		{"a requested delivery date on the order's", func(tx pgx.Tx, s scene) error {
			return all(tx, step(created("requested_delivery_date", "'2026-03-12'"), s.tenant, s.order))
		}, ""},
		{"a requested delivery date before the order's", func(tx pgx.Tx, s scene) error {
			return all(tx, step(created("requested_delivery_date", "'2026-03-11'"), s.tenant, s.order))
		}, "sales_order_requested_delivery"},
		{"a negative change count", func(tx pgx.Tx, s scene) error {
			return all(tx, step(created("changes", "-1"), s.tenant, s.order))
		}, "sales_order_changes_check"},
		{"a cancelled order with its credit block", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, change(s, 1, 3, "none", "blocked"), step(changed, s.order, 1, 120, "blocked"), step(cancel, s.order, "Withdrawn"))...)
		}, ""},
		{"an item's quantity changed while billed", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, append(billed(s), step(price, s.order))...)...)
		}, "something of it is billed, so its quantity and price stay"},
		{"an item's quantity changed while billed, by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, append(billed(s), owner, step(price, s.order))...)...)
		}, "something of it is billed, so its quantity and price stay"},
		{"an item of a cancelled order changed", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, "Once"), step(price, s.order))...)
		}, "item 1: the order is cancelled"},
		{"an item of a cancelled order changed, by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(cancel, s.order, "Once"), owner, step(price, s.order))...)
		}, "item 1: the order is cancelled"},
		{"an item's description changed", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(`UPDATE sales.sales_order_item SET description = 'Bolt'`))...)
		}, "permission denied"},
		{"an item's quantity below what is delivered", func(tx pgx.Tx, s scene) error {
			return all(tx, confirmed(s, append(delivered(s), step(`UPDATE sales.sales_order_item SET quantity = 0.5 WHERE sales_order_id = $1`, s.order))...)...)
		}, "sales_order_item_quantities"},
		{"a change numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "none"), change(s, 1, 3, "none", "none"))...)
		}, "sales_order_change_once"},
		{"a change of no order", func(tx pgx.Tx, s scene) error { return all(tx, change(s, 1, 2, "none", "none")) }, "sales_order_change_order_fkey"},
		{"a change numbered 0", func(tx pgx.Tx, s scene) error { return all(tx, draft(s, change(s, 0, 2, "none", "none"))...) }, "sales_order_change_no_positive"},
		{"a change at the first version", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 1, "none", "none"))...)
		}, "sales_order_change_version_positive"},
		{"a change's reason of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(kept, s.tenant, s.order, 1, 2, " ", 100, 120, "none", "none", "[]"))...)
		}, "sales_order_change_reason_check"},
		{"a change's reason of 501 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(kept, s.tenant, s.order, 1, 2, strings.Repeat("x", 501), 100, 120, "none", "none", "[]"))...)
		}, "sales_order_change_reason_check"},
		{"a change's net value before of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(kept, s.tenant, s.order, 1, 2, nil, 0, 120, "none", "none", "[]"))...)
		}, "sales_order_change_net_before_positive"},
		{"a change's net value after of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(kept, s.tenant, s.order, 1, 2, nil, 100, 0, "none", "none", "[]"))...)
		}, "sales_order_change_net_after_positive"},
		{"a change's unknown block before", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "held", "none"))...)
		}, "sales_order_change_block_before_check"},
		{"a change's unknown block after", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "held"))...)
		}, "sales_order_change_block_after_check"},
		{"a change's items not an array", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, step(kept, s.tenant, s.order, 1, 2, nil, 100, 120, "none", "none", "{}"))...)
		}, "sales_order_change_items_check"},
		{"a change altered", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "none"), owner, step(`UPDATE sales.sales_order_change SET reason = 'Later'`))...)
		}, "is insert-only"},
		{"a change deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, draft(s, change(s, 1, 2, "none", "none"), owner, step(`DELETE FROM sales.sales_order_change`))...)
		}, "is insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), order: id(), doc: id()}
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
