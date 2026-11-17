package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestBillingCancellationRules (T79c, migration 000351; ADR-0067, As
// built T79c): a billing item keeps the pricing its order item had when it
// was billed; a cancellation is one per document, numbered once per
// company, with a reason, and names a credit memo with its entry exactly
// when the document was posted; inserting it cancels the document, takes
// its quantities off what is billed and moves the order's version; the
// amounts billed of an order item count the documents not cancelled; a
// document is cancelled by its cancellation alone, keeping what it posted
// with; a cancellation is never changed.
func TestBillingCancellationRules(t *testing.T) {
	need(t)
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO' || right($2::uuid::text, 12), $3, gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 1000, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 2, 'EA', 500, 1, 1000, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE id = $1`
	const delivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by) VALUES ($1, $3, $2, 1, '2026-03-14', $1)`
	const deliveryItem = `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) VALUES ($1, $3, $2, 1, 2)`
	const document = `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
		currency, net_amount, status, created_by, updated_by)
		SELECT tenant_id, $2, $3, legal_entity_id, customer_id, id, '2026-03-15', currency, 1000, 'created', tenant_id, tenant_id FROM sales.sales_order WHERE id = $1`
	const billingItem = `INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount,
		unit_price) VALUES ($1, $3, 1, $2, 1, 2, 1000, 1)`
	const post = `UPDATE sales.billing_document SET status = 'posted', posting_date = '2026-03-16', customer_invoice_id = gen_random_uuid(),
		customer_invoice_number = 'CI1', journal_entry_year = 2026, journal_entry_id = gen_random_uuid(), version = version + 1 WHERE id = $1`
	const cancel = `INSERT INTO sales.billing_cancellation (tenant_id, id, document_number, legal_entity_id, billing_document_id, sales_order_id,
		cancellation_date, reason, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
		SELECT tenant_id, gen_random_uuid(), $2, legal_entity_id, id, sales_order_id, '2026-03-17', $3, $4, $5, $6, $7, tenant_id
		FROM sales.billing_document WHERE id = $1`
	type scene struct{ tenant, company, order, delivery, doc, memo, entry uuid.UUID }
	var none *string
	var noYear *int16
	var noID *uuid.UUID
	year, memoNumber := int16(2026), "CI2"
	billed := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(order, s.tenant, s.order, s.company), step(item, s.tenant, s.order), step(confirm, s.order),
			step(delivery, s.tenant, s.order, s.delivery), step(deliveryItem, s.tenant, s.order, s.delivery),
			step(document, s.order, s.doc, "BD"+s.doc.String()[24:]), step(billingItem, s.tenant, s.order, s.doc)}, more...)
	}
	unposted := func(s scene) stmt { return step(cancel, s.doc, "BD2", "Too early", noID, none, noYear, noID) }
	reversed := func(s scene) stmt {
		return step(cancel, s.doc, "BD2", "Wrong price", &s.memo, &memoNumber, &year, &s.entry)
	}
	check := func(tx pgx.Tx, query string, want string, args ...any) error {
		var got string
		if err := tx.QueryRow(t.Context(), query, args...).Scan(&got); err != nil {
			return err
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", query, got, want)
		}
		return nil
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a billed item keeps its order item's pricing, whatever the insert gives", func(tx pgx.Tx, s scene) error {
			if err := all(tx, billed(s)...); err != nil {
				return err
			}
			return check(tx, `SELECT format('%s %s %s %s', unit_price, price_unit, price_source, order_net_amount) FROM sales.billing_document_item`,
				"500 1 manual 1000")
		}, ""},
		{"a created document cancelled", func(tx pgx.Tx, s scene) error {
			if err := all(tx, billed(s, unposted(s))...); err != nil {
				return err
			}
			if err := check(tx, `SELECT format('%s %s', status, version) FROM sales.billing_document`, "cancelled 2"); err != nil {
				return err
			}
			return check(tx, `SELECT format('%s %s', trim_scale(i.billed_quantity), o.version) FROM sales.sales_order_item i
				JOIN sales.sales_order o ON o.id = i.sales_order_id`, "0 5")
		}, ""},
		{"a posted document cancelled with its credit memo", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(post, s.doc), reversed(s))...)
		}, ""},
		{"billed again in full once cancelled", func(tx pgx.Tx, s scene) error {
			again := id()
			return all(tx, billed(s, unposted(s), step(document, s.order, again, "BD3"), step(billingItem, s.tenant, s.order, again))...)
		}, ""},
		{"billed again while not cancelled", func(tx pgx.Tx, s scene) error {
			again := id()
			return all(tx, billed(s, step(document, s.order, again, "BD3"), step(billingItem, s.tenant, s.order, again))...)
		}, "sales_order_item_quantities"},
		{"a posted document cancelled without a credit memo", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(post, s.doc), unposted(s))...)
		}, "its document is not a created one, or a posted one with its credit memo"},
		{"a created document cancelled with a credit memo", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, reversed(s))...)
		}, "its document is not a created one, or a posted one with its credit memo"},
		{"cancelled twice", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, unposted(s), step(cancel, s.doc, "BD3", "Again", noID, none, noYear, noID))...)
		}, "billing_cancellation_once"},
		{"a number used twice in the company", func(tx pgx.Tx, s scene) error {
			other := scene{tenant: s.tenant, company: s.company, order: id(), delivery: id(), doc: id()}
			return all(tx, append(billed(s, unposted(s)), billed(other, unposted(other))...)...)
		}, "billing_cancellation_number_once"},
		{"a credit memo without its entry", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(post, s.doc), step(cancel, s.doc, "BD2", "Wrong", &s.memo, &memoNumber, &year, noID))...)
		}, "billing_cancellation_credit_memo"},
		{"a reason of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(cancel, s.doc, "BD2", " ", noID, none, noYear, noID))...)
		}, "billing_cancellation_reason_check"},
		{"a reason of 501 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(cancel, s.doc, "BD2", strings.Repeat("x", 501), noID, none, noYear, noID))...)
		}, "billing_cancellation_reason_check"},
		{"a number of 41 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(cancel, s.doc, strings.Repeat("1", 41), "Wrong", noID, none, noYear, noID))...)
		}, "billing_cancellation_document_number_check"},
		{"a credit memo number of 41 characters", func(tx pgx.Tx, s scene) error {
			long := strings.Repeat("1", 41)
			return all(tx, billed(s, step(post, s.doc), step(cancel, s.doc, "BD2", "Wrong", &s.memo, &long, &year, &s.entry))...)
		}, "billing_cancellation_invoice_number_check"},
		{"a document cancelled without its cancellation", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(`UPDATE sales.billing_document SET status = 'cancelled', version = version + 1 WHERE id = $1`, s.doc))...)
		}, "only a created document changes, and only by being posted"},
		{"a document cancelled without its cancellation, by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.billing_document SET status = 'cancelled', version = version + 1 WHERE id = $1`, s.doc))...)
		}, "only a created document changes, and only by being posted"},
		{"a cancelled document posted", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, unposted(s), step(`SET LOCAL ROLE erp_owner`), step(post, s.doc))...)
		}, "only a created document changes, and only by being posted"},
		{"a cancelled document without its posting", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(post, s.doc), reversed(s), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.billing_document SET posting_date = NULL, customer_invoice_id = NULL, customer_invoice_number = NULL,
					journal_entry_year = NULL, journal_entry_id = NULL, version = version + 1 WHERE id = $1`, s.doc))...)
		}, "only a created document changes, and only by being posted"},
		{"a posted document without its invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(`UPDATE sales.billing_document SET status = 'posted', version = version + 1 WHERE id = $1`, s.doc))...)
		}, "billing_document_posted"},
		{"an item the order does not have", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, step(document, s.order, s.memo, "BD3"),
				step(strings.Replace(billingItem, "1, $2, 1, 2", "1, $2, 9, 2", 1), s.tenant, s.order, s.memo))...)
		}, "billing_document_item_order_item_fkey"},
		{"a cancellation changed", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, unposted(s), step(`SET LOCAL ROLE erp_owner`), step(`UPDATE sales.billing_cancellation SET reason = 'Other'`))...)
		}, "is insert-only"},
		{"a cancellation deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, billed(s, unposted(s), step(`SET LOCAL ROLE erp_owner`), step(`DELETE FROM sales.billing_cancellation`))...)
		}, "is insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), order: id(), delivery: id(), doc: id(), memo: id(), entry: id()}
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
