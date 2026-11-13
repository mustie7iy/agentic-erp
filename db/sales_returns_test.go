package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// returnsScene is a tenant's confirmed sales order SO1 of 5 and 2 EA,
// delivered in full and billed in full by the billing document BD1 (doc),
// with a second billing document of another order (otherDoc) for the
// rules that tell documents apart.
type returnsScene struct {
	tenant, actor, company, customer, order, doc, otherOrder, otherDoc, returns uuid.UUID
}

func newReturnsScene() returnsScene {
	return returnsScene{tenant: id(), actor: id(), company: id(), customer: id(), order: id(), doc: id(), otherOrder: id(), otherDoc: id(), returns: id()}
}

const (
	returnsHeader = `INSERT INTO sales.returns_order (tenant_id, id, document_number, legal_entity_id, customer_id, billing_document_id,
		sales_order_id, returns_date, reason, reason_text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '2026-03-20', $8, $9, $10, $11, $11)`
	returnsLine = `INSERT INTO sales.returns_order_item (tenant_id, returns_order_id, item_no, billing_document_id, billing_item_no, quantity)
		VALUES ($1, $2, $3, $4, $5, $6)`
)

// billedOrder are the statements that make the order with the id and the
// number, of the item quantities, deliver it in full and bill it in full
// with the billing document doc, created.
func (s returnsScene) billedOrder(order, doc uuid.UUID, number string, quantities ...string) []stmt {
	out := []stmt{step(`INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
			currency, payment_term_code, net_amount, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, '2026-03-12', 'EUR', 'NT30', $6, 'draft', $1, $1)`, s.tenant, order, "SO"+number, s.company, s.customer, 100*len(quantities))}
	for i, q := range quantities {
		out = append(out, step(`INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price,
			price_unit, net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', $4, 'EA', 100, 1, 100, gen_random_uuid())`, s.tenant, order, i+1, q))
	}
	delivery := id()
	out = append(out, step(`UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`, order),
		step(`INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by) VALUES ($1, $2, $3, 1, '2026-03-14', $1)`,
			s.tenant, delivery, order),
		step(`INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity)
			SELECT tenant_id, $1, sales_order_id, item_no, quantity FROM sales.sales_order_item WHERE sales_order_id = $2`, delivery, order),
		step(`INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
			currency, net_amount, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-03-15', 'EUR', $7, 'created', $1, $1)`,
			s.tenant, doc, "BD"+number, s.company, s.customer, order, 100*len(quantities)),
		step(`INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
			SELECT tenant_id, $1, item_no, sales_order_id, item_no, quantity, net_amount FROM sales.sales_order_item WHERE sales_order_id = $2`, doc, order))
	return out
}

// post posts the billing document.
func (s returnsScene) post(doc uuid.UUID) stmt {
	return step(`UPDATE sales.billing_document SET status = 'posted', posting_date = '2026-03-16', customer_invoice_id = gen_random_uuid(),
		customer_invoice_number = 'CI1', journal_entry_year = 2026, journal_entry_id = gen_random_uuid(), version = version + 1, updated_by = tenant_id
		WHERE id = $1`, doc)
}

// billed is the scene's order billed and posted, and the other order
// billed and posted too, followed by more.
func (s returnsScene) billed(more ...stmt) []stmt {
	out := append(s.billedOrder(s.order, s.doc, "1", "5", "2"), s.post(s.doc))
	out = append(out, s.billedOrder(s.otherOrder, s.otherDoc, "2", "5")...)
	return append(append(out, s.post(s.otherDoc)), more...)
}

// header is a returns order of the scene's billing document: id, number,
// reason and status.
func (s returnsScene) header(returns uuid.UUID, number, reason, status string) stmt {
	return step(returnsHeader, s.tenant, returns, number, s.company, s.customer, s.doc, s.order, reason, nil, status, s.actor)
}

// made is the scene's returns order, a draft.
func (s returnsScene) made() stmt { return s.header(s.returns, "RO1", "defective", "draft") }

// line is an item of the returns order: its number, the billing item and
// the quantity.
func (s returnsScene) line(returns uuid.UUID, no, billingItem int, quantity string) stmt {
	return step(returnsLine, s.tenant, returns, no, s.doc, billingItem, quantity)
}

// shows fails unless the scene's sales order has this version, was
// changed last by this actor and its items have these returned
// quantities.
func (s returnsScene) shows(tx pgx.Tx, version int, by uuid.UUID, returned string) error {
	var got string
	want := fmt.Sprintf("version %d by %s, returned %s", version, by, returned)
	err := tx.QueryRow(context.Background(), `SELECT format('version %s by %s, returned %s', o.version, o.updated_by,
			(SELECT string_agg(trim_scale(i.returned_quantity)::text, ' ' ORDER BY i.item_no) FROM sales.sales_order_item i WHERE i.sales_order_id = o.id))
		FROM sales.sales_order o WHERE o.id = $1`, s.order).Scan(&got)
	if err == nil && got != want {
		err = fmt.Errorf("the sales order: got %s, want %s", got, want)
	}
	return err
}

// TestReturnsOrderRules (T85a, migration 000306; ADR-0067, As built
// T85a): a returns order is for a posted billing document of its company
// and customer with that document's sales order, numbered once per
// company, for a known reason; it is a draft confirmed once and never
// otherwise changed or deleted; its items take quantities above zero of
// the billing document's items, each once, and all the document's
// returns orders together at most what an item billed; each item adds to
// what is returned of the sales order's item, which only the database
// moves, and each returns order moves the sales order's version once.
func TestReturnsOrderRules(t *testing.T) {
	need(t)
	const confirm = `UPDATE sales.returns_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s returnsScene) error
		want string // empty: allowed
	}{
		{"a returns order adds to what is returned and moves the sales order's version once", func(tx pgx.Tx, s returnsScene) error {
			// The order is at version 4 once created, confirmed, delivered and billed.
			if err := all(tx, s.billed(s.made(), s.line(s.returns, 1, 2, "2"), s.line(s.returns, 2, 1, "1.5"))...); err != nil {
				return err
			}
			return s.shows(tx, 5, s.actor, "1.5 2")
		}, ""},
		{"a second returns order adds to the first", func(tx pgx.Tx, s returnsScene) error {
			second := id()
			if err := all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "1.5"), s.header(second, "RO2", "other", "draft"),
				s.line(second, 1, 1, "3.5"))...); err != nil {
				return err
			}
			return s.shows(tx, 6, s.actor, "5 0")
		}, ""},
		{"a draft confirmed", func(tx pgx.Tx, s returnsScene) error { return all(tx, s.billed(s.made(), step(confirm, s.returns))...) }, ""},
		{"a reason in words", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", s.company, s.customer, s.doc, s.order, "other", "Late", "draft", s.actor))...)
		}, ""},
		{"more than was billed", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "5.000001"))...)
		}, "returned of 5.000000 billed"},
		{"more than is left after an earlier returns order", func(tx pgx.Tx, s returnsScene) error {
			second := id()
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "4"), s.header(second, "RO2", "other", "draft"), s.line(second, 1, 1, "1.5"))...)
		}, "returned of 5.000000 billed"},
		{"what is returned above what is billed, by the tables' owner", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(`SET LOCAL ROLE erp_owner`), step(`UPDATE sales.sales_order_item SET returned_quantity = 5.5 WHERE sales_order_id = $1 AND item_no = 1`, s.order))...)
		}, "sales_order_item_returned"},
		{"what is returned below zero, by the tables' owner", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(`SET LOCAL ROLE erp_owner`), step(`UPDATE sales.sales_order_item SET returned_quantity = -1 WHERE sales_order_id = $1 AND item_no = 1`, s.order))...)
		}, "sales_order_item_returned"},
		{"a number of 41 characters", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.header(s.returns, strings.Repeat("1", 41), "defective", "draft"))...)
		}, "returns_order_document_number_check"},
		{"an empty number", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.header(s.returns, "", "defective", "draft"))...)
		}, "returns_order_document_number_check"},
		{"a reason text of 501 characters", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", s.company, s.customer, s.doc, s.order, "other", strings.Repeat("x", 501), "draft", s.actor))...)
		}, "returns_order_reason_text_check"},
		{"an item numbered zero", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 0, 1, "1"))...)
		}, "returns_order_item_item_no_check"},
		{"a billing document that is not posted", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, append(s.billedOrder(s.order, s.doc, "1", "5", "2"), s.made())...)
		}, "its billing document is not a posted one"},
		{"another customer than the billing document's", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", s.company, id(), s.doc, s.order, "defective", nil, "draft", s.actor))...)
		}, "its billing document is not a posted one"},
		{"another company than the billing document's", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", id(), s.customer, s.doc, s.order, "defective", nil, "draft", s.actor))...)
		}, "its billing document is not a posted one"},
		{"another sales order than the billing document's", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", s.company, s.customer, s.doc, s.otherOrder, "defective", nil, "draft", s.actor))...)
		}, "returns_order_billing_document_fkey"},
		{"no billing document", func(tx pgx.Tx, s returnsScene) error { return all(tx, s.made()) }, "returns_order_billing_document_fkey"},
		{"a number issued twice in the company", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.header(id(), "RO1", "defective", "draft"))...)
		}, "returns_order_number_once"},
		{"an unknown reason", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.header(s.returns, "RO1", "bored", "draft"))...)
		}, "returns_order_reason_check"},
		{"a reason text of white space", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(returnsHeader, s.tenant, s.returns, "RO1", s.company, s.customer, s.doc, s.order, "other", " ", "draft", s.actor))...)
		}, "returns_order_reason_text_check"},
		{"an unknown status", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.header(s.returns, "RO1", "defective", "received"))...)
		}, "returns_order_status_check"},
		{"confirmed twice", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), step(confirm, s.returns), step(confirm, s.returns))...)
		}, "only a draft changes"},
		{"a draft's version moved without confirming it", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), step(`UPDATE sales.returns_order SET version = version + 1 WHERE id = $1`, s.returns))...)
		}, "only a draft changes"},
		{"the reason changed", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), step(`UPDATE sales.returns_order SET reason = 'other'`))...)
		}, "permission denied"},
		{"a returns order deleted", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), step(`DELETE FROM sales.returns_order`))...)
		}, "permission denied"},
		{"a quantity of zero", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "0"))...)
		}, "returns_order_item_quantity_positive"},
		{"a billing item twice in a returns order", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "1"), s.line(s.returns, 2, 1, "1"))...)
		}, "returns_order_item_once"},
		{"an item the billing document does not have", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 3, "1"))...)
		}, "returns_order_item_billing_item_fkey"},
		{"an item of another billing document than the returns order's", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), step(returnsLine, s.tenant, s.returns, 1, s.otherDoc, 1, "1"))...)
		}, "returns_order_item_order_fkey"},
		{"an item of no returns order", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.line(s.returns, 1, 1, "1"))...)
		}, "returns_order_item_order_fkey"},
		{"a returns order's item changed", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "1"), step(`UPDATE sales.returns_order_item SET quantity = 2`))...)
		}, "permission denied"},
		{"a returns order's item deleted", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "1"), step(`DELETE FROM sales.returns_order_item`))...)
		}, "permission denied"},
		{"what is returned set by the application itself", func(tx pgx.Tx, s returnsScene) error {
			return all(tx, s.billed(step(`UPDATE sales.sales_order_item SET returned_quantity = 1`))...)
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newReturnsScene()
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
	t.Run("another tenant sees none", func(t *testing.T) {
		s := newReturnsScene()
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, s.billed(s.made(), s.line(s.returns, 1, 1, "1"), step(`SELECT set_config('app.tenant_id', $1, true)`, id().String()))...); err != nil {
				return err
			}
			var orders, items int
			if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM sales.returns_order), (SELECT count(*) FROM sales.returns_order_item)`).Scan(&orders, &items); err != nil {
				return err
			}
			if orders != 0 || items != 0 {
				return fmt.Errorf("another tenant reads %d returns orders and %d items, want none", orders, items)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestReturnReceiptRules (T85b, migration 000307; ADR-0067, As built
// T85b): a receipt is of a confirmed returns order, numbered once within
// it, and never changed or deleted; its items take quantities above zero
// of the returns order's items, each once per receipt, and all the
// receipts together at most what an item returns; each item adds to what
// is received of the returns order's item, which only the database
// moves, and each receipt moves the returns order's version once, the
// one change of a confirmed returns order, which the application cannot
// make itself; the sales order is not touched.
func TestReturnReceiptRules(t *testing.T) {
	need(t)
	const confirm = `UPDATE sales.returns_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	const receipt = `INSERT INTO sales.return_receipt (tenant_id, id, returns_order_id, receipt_no, receipt_date, created_by)
		VALUES ($1, $2, $3, $4, '2026-03-22', $5)`
	const line = `INSERT INTO sales.return_receipt_item (tenant_id, receipt_id, returns_order_id, item_no, quantity) VALUES ($1, $2, $3, $4, $5)`
	// returned is the scene's returns order of 4 of billing item 1 and 2 of billing item 2, confirmed, and another, a
	// draft, of 1 of billing item 1, followed by more.
	returned := func(s returnsScene, draft uuid.UUID, more ...stmt) []stmt {
		return s.billed(append([]stmt{s.made(), s.line(s.returns, 1, 1, "4"), s.line(s.returns, 2, 2, "2"), step(confirm, s.returns),
			s.header(draft, "RO2", "other", "draft"), s.line(draft, 1, 1, "1")}, more...)...)
	}
	// shows fails unless the returns order's version, the actor who changed it last and its items' received quantities
	// are these, and the sales order is at the version the two returns orders left it at.
	shows := func(tx pgx.Tx, s returnsScene, version int, by uuid.UUID, received string) error {
		var got string
		want := fmt.Sprintf("version %d by %s, received %s, sales order version 6", version, by, received)
		err := tx.QueryRow(context.Background(), `SELECT format('version %s by %s, received %s, sales order version %s', r.version, r.updated_by,
				(SELECT string_agg(trim_scale(i.received_quantity)::text, ' ' ORDER BY i.item_no) FROM sales.returns_order_item i WHERE i.returns_order_id = r.id),
				(SELECT o.version FROM sales.sales_order o WHERE o.id = r.sales_order_id))
			FROM sales.returns_order r WHERE r.id = $1`, s.returns).Scan(&got)
		if err == nil && got != want {
			err = fmt.Errorf("the returns order: got %s, want %s", got, want)
		}
		return err
	}
	type scene struct {
		returnsScene
		draft, receipt, receiver uuid.UUID
	}
	made := func(s scene) stmt { return step(receipt, s.tenant, s.receipt, s.returns, 1, s.receiver) }
	with := func(s scene, more ...stmt) []stmt {
		return returned(s.returnsScene, s.draft, append([]stmt{made(s)}, more...)...)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a receipt adds to what is received and moves the returns order's version once", func(tx pgx.Tx, s scene) error {
			if err := all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1.5"), step(line, s.tenant, s.receipt, s.returns, 2, "2"))...); err != nil {
				return err
			}
			return shows(tx, s.returnsScene, 3, s.receiver, "1.5 2")
		}, ""},
		{"a second receipt adds to the first", func(tx pgx.Tx, s scene) error {
			second := id()
			if err := all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1.5"), step(receipt, s.tenant, second, s.returns, 2, s.receiver),
				step(line, s.tenant, second, s.returns, 1, "2.5"))...); err != nil {
				return err
			}
			return shows(tx, s.returnsScene, 4, s.receiver, "4 0")
		}, ""},
		{"more than is returned", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "4.000001"))...)
		}, "returns_order_item_received"},
		{"more than is open after an earlier receipt", func(tx pgx.Tx, s scene) error {
			second := id()
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "3"), step(receipt, s.tenant, second, s.returns, 2, s.receiver),
				step(line, s.tenant, second, s.returns, 1, "1.5"))...)
		}, "returns_order_item_received"},
		{"what is received below zero, by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.returns_order_item SET received_quantity = -1 WHERE returns_order_id = $1 AND item_no = 1`, s.returns))...)
		}, "returns_order_item_received"},
		{"a receipt of a draft returns order", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(receipt, s.tenant, s.receipt, s.draft, 1, s.receiver))...)
		}, "its returns order is not confirmed"},
		{"a receipt of no returns order", func(tx pgx.Tx, s scene) error {
			return all(tx, step(receipt, s.tenant, s.receipt, s.returns, 1, s.receiver))
		}, "return_receipt_returns_order_fkey"},
		{"a number used twice in the returns order", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(receipt, s.tenant, id(), s.returns, 1, s.receiver))...)
		}, "return_receipt_no_once"},
		{"a number of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(receipt, s.tenant, s.receipt, s.returns, 0, s.receiver))...)
		}, "return_receipt_no_positive"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "0"))...)
		}, "return_receipt_item_quantity_positive"},
		{"a returns order item twice in a receipt", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1"), step(line, s.tenant, s.receipt, s.returns, 1, "1"))...)
		}, "return_receipt_item_pkey"},
		{"an item the returns order does not have", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 3, "1"))...)
		}, "return_receipt_item_order_item_fkey"},
		{"an item of another returns order than the receipt's", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.draft, 1, "1"))...)
		}, "return_receipt_item_receipt_fkey"},
		{"an item of no receipt", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(line, s.tenant, s.receipt, s.returns, 1, "1"))...)
		}, "return_receipt_item_receipt_fkey"},
		{"a receipt changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`UPDATE sales.return_receipt SET receipt_date = '2026-03-23'`))...)
		}, "permission denied"},
		{"a receipt deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`DELETE FROM sales.return_receipt`))...)
		}, "permission denied"},
		{"a receipt's item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1"), step(`UPDATE sales.return_receipt_item SET quantity = 2`))...)
		}, "permission denied"},
		{"a receipt's item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1"), step(`DELETE FROM sales.return_receipt_item`))...)
		}, "permission denied"},
		{"what is received set by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(`UPDATE sales.returns_order_item SET received_quantity = 1`))...)
		}, "permission denied"},
		{"a confirmed returns order's version moved by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, returned(s.returnsScene, s.draft, step(`UPDATE sales.returns_order SET version = version + 1 WHERE id = $1`, s.returns))...)
		}, "only a draft changes"},
		{"a receipt changed by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`SET LOCAL ROLE erp_owner`), step(`UPDATE sales.return_receipt SET receipt_date = '2026-03-23'`))...)
		}, "is insert-only"},
		{"a receipt deleted by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`SET LOCAL ROLE erp_owner`), step(`DELETE FROM sales.return_receipt`))...)
		}, "is insert-only"},
		{"a receipt's item changed by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1"), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE sales.return_receipt_item SET quantity = 2`))...)
		}, "is insert-only"},
		{"a receipt's item deleted by the tables' owner", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.receipt, s.returns, 1, "1"), step(`SET LOCAL ROLE erp_owner`),
				step(`DELETE FROM sales.return_receipt_item`))...)
		}, "is insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{returnsScene: newReturnsScene(), draft: id(), receipt: id(), receiver: id()}
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
