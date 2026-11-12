package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesPermissions (T78a, migration 000276; T78b, migration 000277;
// T78c, migration 000278; T79a, migration 000279; ADR-0067 §7): the sales
// permissions are seeded and are no approval permissions.
func TestSalesPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'sales.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "sales.billing.post sales.billing.read sales.billing.write sales.delivery.confirm sales.order.confirm sales.order.read sales.order.write sales.quotation.read sales.quotation.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestQuotationRules (T78a, migration 000276; ADR-0067 §2, §3): a
// quotation is created open, numbered once per company, valid until a day
// not before its date, with a net value above zero; the customer's answer
// is its one change, once; an item is a positive quantity in a known unit
// at a positive price per a positive price unit with a positive net
// value; what was offered is fixed and never deleted.
func TestQuotationRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), '2026-03-10', $5, 'EUR', $6, $7, $8, $8)`
	const item = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', $4, $5, $6, $7, $8, gen_random_uuid())`
	const answer = `UPDATE sales.quotation SET status = $1, version = version + 1, updated_by = tenant_id`
	type scene struct{ tenant, actor, company, doc uuid.UUID }
	quotation := func(s scene, doc uuid.UUID, number, validUntil string, net int64, status string) stmt {
		return step(header, s.tenant, doc, number, s.company, validUntil, net, status, s.actor)
	}
	made := func(s scene) stmt { return quotation(s, s.doc, "QT1", "2026-04-10", 10000, "open") }
	line := func(s scene, no int, quantity, unit string, price int64, priceUnit int, net int64) stmt {
		return step(item, s.tenant, s.doc, no, quantity, unit, price, priceUnit, net)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an open quotation with its items", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "2.5", "KG", 1001, 1, 2503), line(s, 2, "3", "EA", 9999, 100, 300))
		}, ""},
		{"valid on its own day alone", func(tx pgx.Tx, s scene) error { return all(tx, quotation(s, s.doc, "QT1", "2026-03-10", 1, "open")) }, ""},
		{"valid until the day before it is made", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-03-09", 1, "open"))
		}, "quotation_validity"},
		{"created in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-04-10", 1, "expired"))
		}, "quotation_status_check"},
		{"a net value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-04-10", 0, "open"))
		}, "quotation_net_positive"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), quotation(s, id(), "QT1", "2026-04-10", 1, "open"))
		}, "quotation_number_once"},
		{"the same number in another company", func(tx pgx.Tx, s scene) error {
			other := s
			other.company = id()
			return all(tx, made(s), quotation(other, id(), "QT1", "2026-04-10", 1, "open"))
		}, ""},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(strings.Replace(header, "'EUR'", "'XXA'", 1), s.tenant, s.doc, "QT1", s.company, "2026-04-10", 1, "open", s.actor))
		}, "quotation_currency_fkey"},
		{"accepted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(answer, "accepted")) }, ""},
		{"rejected", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(answer, "rejected")) }, ""},
		{"accepted, then rejected", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "accepted"), step(answer, "rejected"))
		}, "only an open quotation changes"},
		{"rejected, then accepted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "rejected"), step(answer, "accepted"))
		}, "only an open quotation changes"},
		{"an open quotation changed without an answer", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "open"))
		}, "only an open quotation changes"},
		{"what was offered changed with the answer", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(`UPDATE sales.quotation SET status = 'accepted', net_amount = 1, valid_until = '2027-01-01'`))
		}, "permission denied"},
		{"a quotation deleted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(`DELETE FROM sales.quotation`)) }, "permission denied"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "0", "EA", 100, 1, 100))
		}, "quotation_item_quantity_positive"},
		{"an unknown unit", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "XX", 100, 1, 100))
		}, "quotation_item_uom_code_fkey"},
		{"a price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 0, 1, 100))
		}, "quotation_item_price_positive"},
		{"a price unit of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 0, 100))
		}, "quotation_item_price_unit_positive"},
		{"an item's value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 0))
		}, "quotation_item_net_positive"},
		{"a description of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(strings.Replace(item, "'Bracket'", "'  '", 1), s.tenant, s.doc, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_description_check"},
		{"an item key of white space", func(tx pgx.Tx, s scene) error {
			keyed := strings.Replace(strings.Replace(item, "description,", "description, item_key,", 1), "'Bracket',", "'Bracket', ' ',", 1)
			return all(tx, made(s), step(keyed, s.tenant, s.doc, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_item_key_check"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), line(s, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_pkey"},
		{"an item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 0, "1", "EA", 100, 1, 100))
		}, "quotation_item_item_no_check"},
		{"an item of no quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, line(s, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_tenant_id_quotation_id_fkey"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), step(`UPDATE sales.quotation_item SET net_amount = 200`))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), step(`DELETE FROM sales.quotation_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), company: id(), doc: id()}
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

// TestSalesOrderRules (T78b, migration 000277; ADR-0067 §5): a sales
// order is created as a draft, numbered once per company, with a payment
// term and a net value above zero; from a quotation only an accepted one
// of its company, customer and currency, and one order per quotation;
// confirming is its one change, once; an item is a quotation item's
// shape with delivered and billed quantities that never exceed what was
// ordered and delivered, and the application changes none.
func TestSalesOrderRules(t *testing.T) {
	need(t)
	const quotation = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '2026-03-10', '2026-04-10', 'EUR', 100, 'open', $1, $1)`
	const accept = `UPDATE sales.quotation SET status = $2, version = version + 1, updated_by = tenant_id WHERE id = $1`
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, '2026-03-12', $7, $8, $9, $10, $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id, delivered_quantity, billed_quantity) VALUES ($1, $2, $3, 'Bracket', $4, $5, $6, $7, $8, gen_random_uuid(), $9, $10)`
	const confirm = `UPDATE sales.sales_order SET status = $1, version = version + 1, updated_by = tenant_id`
	type scene struct{ tenant, company, customer, doc, quotation uuid.UUID }
	var direct *uuid.UUID
	order := func(s scene, doc uuid.UUID, number string, from *uuid.UUID, currency, term string, net int64, status string) stmt {
		return step(header, s.tenant, doc, number, s.company, s.customer, from, currency, term, net, status)
	}
	made := func(s scene) stmt { return order(s, s.doc, "SO1", direct, "EUR", "NT30", 100, "draft") }
	quoted := func(s scene, status string) []stmt {
		out := []stmt{step(quotation, s.tenant, s.quotation, "QT1", s.company, s.customer)}
		if status != "open" {
			out = append(out, step(accept, s.quotation, status))
		}
		return out
	}
	line := func(s scene, no int, quantity, unit string, price int64, priceUnit int, net int64, delivered, billed string) stmt {
		return step(item, s.tenant, s.doc, no, quantity, unit, price, priceUnit, net, delivered, billed)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a draft with its items", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "2.5", "KG", 1001, 1, 2503, "0", "0"), line(s, 2, "3", "EA", 9999, 100, 300, "3", "2"))
		}, ""},
		{"from an accepted quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, append(quoted(s, "accepted"), order(s, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, ""},
		{"from an open quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, append(quoted(s, "open"), order(s, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, "its quotation is not an accepted one"},
		{"from a rejected quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, append(quoted(s, "rejected"), order(s, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, "its quotation is not an accepted one"},
		{"from a quotation in another currency", func(tx pgx.Tx, s scene) error {
			return all(tx, append(quoted(s, "accepted"), order(s, s.doc, "SO1", &s.quotation, "USD", "NT30", 100, "draft"))...)
		}, "its quotation is not an accepted one"},
		{"from a quotation to another customer", func(tx pgx.Tx, s scene) error {
			other := s
			other.customer = id()
			return all(tx, append(quoted(s, "accepted"), order(other, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, "its quotation is not an accepted one"},
		{"from a quotation of another company", func(tx pgx.Tx, s scene) error {
			other := s
			other.company = id()
			return all(tx, append(quoted(s, "accepted"), order(other, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, "its quotation is not an accepted one"},
		{"from a quotation that does not exist", func(tx pgx.Tx, s scene) error {
			return all(tx, order(s, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"))
		}, "its quotation is not an accepted one"},
		{"a second order of a quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, append(quoted(s, "accepted"), order(s, s.doc, "SO1", &s.quotation, "EUR", "NT30", 100, "draft"),
				order(s, id(), "SO2", &s.quotation, "EUR", "NT30", 100, "draft"))...)
		}, "sales_order_one_per_quotation"},
		{"two orders created directly", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), order(s, id(), "SO2", direct, "EUR", "NT30", 100, "draft"))
		}, ""},
		{"created in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, order(s, s.doc, "SO1", direct, "EUR", "NT30", 100, "cancelled"))
		}, "sales_order_status_check"},
		{"a net value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, order(s, s.doc, "SO1", direct, "EUR", "NT30", 0, "draft"))
		}, "sales_order_net_positive"},
		{"an empty payment term", func(tx pgx.Tx, s scene) error {
			return all(tx, order(s, s.doc, "SO1", direct, "EUR", "", 100, "draft"))
		}, "sales_order_payment_term_code_check"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), order(s, id(), "SO1", direct, "EUR", "NT30", 100, "draft"))
		}, "sales_order_number_once"},
		{"the same number in another company", func(tx pgx.Tx, s scene) error {
			other := s
			other.company = id()
			return all(tx, made(s), order(other, id(), "SO1", direct, "EUR", "NT30", 100, "draft"))
		}, ""},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, order(s, s.doc, "SO1", direct, "XXA", "NT30", 100, "draft"))
		}, "sales_order_currency_fkey"},
		{"confirmed", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(confirm, "confirmed")) }, ""},
		{"confirmed twice", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(confirm, "confirmed"), step(confirm, "confirmed"))
		}, "only a draft changes"},
		{"a draft changed without being confirmed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(confirm, "draft"))
		}, "only a draft changes"},
		{"what was ordered changed with the confirmation", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(`UPDATE sales.sales_order SET status = 'confirmed', net_amount = 1, payment_term_code = 'NT14'`))
		}, "permission denied"},
		{"an order deleted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(`DELETE FROM sales.sales_order`)) }, "permission denied"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "0", "EA", 100, 1, 100, "0", "0"))
		}, "sales_order_item_quantity_positive"},
		{"an unknown unit", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "XX", 100, 1, 100, "0", "0"))
		}, "sales_order_item_uom_code_fkey"},
		{"a price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 0, 1, 100, "0", "0"))
		}, "sales_order_item_price_positive"},
		{"a price unit of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 0, 100, "0", "0"))
		}, "sales_order_item_price_unit_positive"},
		{"an item's value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 0, "0", "0"))
		}, "sales_order_item_net_positive"},
		{"a description of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(strings.Replace(item, "'Bracket'", "'  '", 1), s.tenant, s.doc, 1, "1", "EA", 100, 1, 100, "0", "0"))
		}, "sales_order_item_description_check"},
		{"an item key of white space", func(tx pgx.Tx, s scene) error {
			keyed := strings.Replace(strings.Replace(item, "description,", "description, item_key,", 1), "'Bracket',", "'Bracket', ' ',", 1)
			return all(tx, made(s), step(keyed, s.tenant, s.doc, 1, "1", "EA", 100, 1, 100, "0", "0"))
		}, "sales_order_item_item_key_check"},
		{"more delivered than ordered", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100, "1.000001", "0"))
		}, "sales_order_item_quantities"},
		{"more billed than delivered", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "2", "EA", 100, 1, 200, "1", "1.5"))
		}, "sales_order_item_quantities"},
		{"a negative billed quantity", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "2", "EA", 100, 1, 200, "1", "-1"))
		}, "sales_order_item_quantities"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100, "0", "0"), line(s, 1, "1", "EA", 100, 1, 100, "0", "0"))
		}, "sales_order_item_pkey"},
		{"an item of no order", func(tx pgx.Tx, s scene) error {
			return all(tx, line(s, 1, "1", "EA", 100, 1, 100, "0", "0"))
		}, "sales_order_item_tenant_id_sales_order_id_fkey"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100, "0", "0"), step(`UPDATE sales.sales_order_item SET delivered_quantity = 1`))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100, "0", "0"), step(`DELETE FROM sales.sales_order_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), customer: id(), doc: id(), quotation: id()}
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

// TestDeliveryRules (T78c, migration 000278; ADR-0067 §8): only a
// confirmed order takes a delivery, numbered once within it; an item is a
// quantity above zero of an item of the delivery's own order, once per
// delivery; each item adds its quantity to the order item's delivered
// quantity, never above what was ordered, and each delivery moves the
// order's version once; a delivery is never changed or deleted, and the
// application still changes neither a confirmed order nor its items
// itself.
func TestDeliveryRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', $4, 'EA', 100, 1, 100, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	const delivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		VALUES ($1, $2, $3, $4, '2026-03-14', $5)`
	const line = `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) VALUES ($1, $2, $3, $4, $5)`
	type scene struct{ tenant, actor, order, other, delivery uuid.UUID }
	// ordered is a confirmed order of 5 and 2 EA, and another, a draft, of 5 EA.
	ordered := func(s scene) []stmt {
		return []stmt{step(header, s.tenant, s.order, "SO1"), step(item, s.tenant, s.order, 1, "5"), step(item, s.tenant, s.order, 2, "2"),
			step(confirm, s.order), step(header, s.tenant, s.other, "SO2"), step(item, s.tenant, s.other, 1, "5")}
	}
	made := func(s scene) stmt { return step(delivery, s.tenant, s.delivery, s.order, 1, s.actor) }
	with := func(s scene, more ...stmt) []stmt { return append(append(ordered(s), made(s)), more...) }
	// shows fails unless the order's version, the actor who changed it last and its items' delivered quantities are these.
	shows := func(tx pgx.Tx, s scene, version int, by uuid.UUID, delivered string) error {
		var got, want string
		want = fmt.Sprintf("version %d by %s, delivered %s", version, by, delivered)
		err := tx.QueryRow(context.Background(), `SELECT format('version %s by %s, delivered %s', o.version, o.updated_by,
				(SELECT string_agg(trim_scale(i.delivered_quantity)::text, ' ' ORDER BY i.item_no) FROM sales.sales_order_item i WHERE i.sales_order_id = o.id))
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
		{"a delivery adds to what is delivered and moves the order's version once", func(tx pgx.Tx, s scene) error {
			if err := all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "1.5"), step(line, s.tenant, s.delivery, s.order, 2, "2"))...); err != nil {
				return err
			}
			return shows(tx, s, 3, s.actor, "1.5 2")
		}, ""},
		{"a second delivery adds to the first", func(tx pgx.Tx, s scene) error {
			second := id()
			if err := all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "1.5"), step(delivery, s.tenant, second, s.order, 2, s.actor),
				step(line, s.tenant, second, s.order, 1, "3.5"))...); err != nil {
				return err
			}
			return shows(tx, s, 4, s.actor, "5 0")
		}, ""},
		{"more than was ordered", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "5.000001"))...)
		}, "sales_order_item_quantities"},
		{"more than is open after an earlier delivery", func(tx pgx.Tx, s scene) error {
			second := id()
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "4"), step(delivery, s.tenant, second, s.order, 2, s.actor),
				step(line, s.tenant, second, s.order, 1, "1.5"))...)
		}, "sales_order_item_quantities"},
		{"a delivery of a draft order", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(delivery, s.tenant, s.delivery, s.other, 1, s.actor))...)
		}, "its sales order is not confirmed"},
		{"a delivery of no order", func(tx pgx.Tx, s scene) error {
			return all(tx, step(delivery, s.tenant, s.delivery, s.order, 1, s.actor))
		}, "delivery_sales_order_fkey"},
		{"a number used twice in the order", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(delivery, s.tenant, id(), s.order, 1, s.actor))...)
		}, "delivery_no_once"},
		{"a number of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(delivery, s.tenant, s.delivery, s.order, 0, s.actor))...)
		}, "delivery_no_positive"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "0"))...)
		}, "delivery_item_quantity_positive"},
		{"an order item twice in a delivery", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "1"), step(line, s.tenant, s.delivery, s.order, 1, "1"))...)
		}, "delivery_item_pkey"},
		{"an item the order does not have", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 3, "1"))...)
		}, "delivery_item_order_item_fkey"},
		{"an item of another order than the delivery's", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.other, 1, "1"))...)
		}, "delivery_item_delivery_fkey"},
		{"an item of no delivery", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(line, s.tenant, s.delivery, s.order, 1, "1"))...)
		}, "delivery_item_delivery_fkey"},
		{"a delivery changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`UPDATE sales.delivery SET delivery_date = '2026-03-15'`))...)
		}, "permission denied"},
		{"a delivery deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`DELETE FROM sales.delivery`))...)
		}, "permission denied"},
		{"a delivery's item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "1"), step(`UPDATE sales.delivery_item SET quantity = 2`))...)
		}, "permission denied"},
		{"a delivery's item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.delivery, s.order, 1, "1"), step(`DELETE FROM sales.delivery_item`))...)
		}, "permission denied"},
		{"what is delivered set by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(`UPDATE sales.sales_order_item SET delivered_quantity = 1`))...)
		}, "permission denied"},
		{"a confirmed order's version moved by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(`UPDATE sales.sales_order SET version = version + 1 WHERE id = $1`, s.order))...)
		}, "only a draft changes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), order: id(), other: id(), delivery: id()}
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

// TestBillingRules (T79a, migration 000279; ADR-0067 §9): a billing
// document is created for a confirmed order of its company, customer and
// currency, numbered once per company, with a net value above zero; it
// changes only by being posted (TestBillingPostingRules) and is never
// deleted; an item is a quantity and an amount above
// zero of an item of the document's own order, once per document; each
// item adds its quantity to the order item's billed quantity, never above
// what is delivered; the amounts billed of an order item never exceed its
// net value and are its net value once it is billed in full; each
// document moves the order's version once.
func TestBillingRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, '2026-03-12', 'EUR', 'NT30', 2000, 'draft', $1, $1)`
	const item = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', $4, 'EA', 1000, 3, 1000, gen_random_uuid())`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	const delivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
		VALUES ($1, $2, $3, $4, '2026-03-14', $1)`
	const shipped = `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) VALUES ($1, $2, $3, $4, $5)`
	const document = `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
		currency, net_amount, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-03-15', $7, $8, $9, $10, $10)`
	const line = `INSERT INTO sales.billing_document_item (tenant_id, billing_document_id, item_no, sales_order_id, order_item_no, quantity, net_amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	type scene struct{ tenant, actor, company, customer, order, other, doc uuid.UUID }
	// ordered is a confirmed order of two items of 3 EA worth 10.00 each, 2 and 3 of them delivered, and another, a draft.
	ordered := func(s scene) []stmt {
		shipment := id()
		return []stmt{step(header, s.tenant, s.order, "SO1", s.company, s.customer), step(item, s.tenant, s.order, 1, "3"), step(item, s.tenant, s.order, 2, "3"),
			step(confirm, s.order), step(delivery, s.tenant, shipment, s.order, 1), step(shipped, s.tenant, shipment, s.order, 1, "2"),
			step(shipped, s.tenant, shipment, s.order, 2, "3"), step(header, s.tenant, s.other, "SO2", s.company, s.customer),
			step(item, s.tenant, s.other, 1, "3")}
	}
	bill := func(s scene, doc uuid.UUID, number string, order uuid.UUID, currency string, net int64, status string) stmt {
		return step(document, s.tenant, doc, number, s.company, s.customer, order, currency, net, status, s.actor)
	}
	made := func(s scene) stmt { return bill(s, s.doc, "BD1", s.order, "EUR", 1667, "created") }
	with := func(s scene, more ...stmt) []stmt { return append(append(ordered(s), made(s)), more...) }
	// shows fails unless the order's version, the actor who changed it last and its items' billed quantities are these.
	shows := func(tx pgx.Tx, s scene, version int, by uuid.UUID, billed string) error {
		var got string
		want := fmt.Sprintf("version %d by %s, billed %s", version, by, billed)
		err := tx.QueryRow(context.Background(), `SELECT format('version %s by %s, billed %s', o.version, o.updated_by,
				(SELECT string_agg(trim_scale(i.billed_quantity)::text, ' ' ORDER BY i.item_no) FROM sales.sales_order_item i WHERE i.sales_order_id = o.id))
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
		{"a document adds to what is billed and moves the order's version once", func(tx pgx.Tx, s scene) error {
			if err := all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "2", 667), step(line, s.tenant, s.doc, 2, s.order, 2, "3", 1000))...); err != nil {
				return err
			}
			return shows(tx, s, 4, s.actor, "2 3")
		}, ""},
		{"a second document bills the rest of an item at the remainder", func(tx pgx.Tx, s scene) error {
			shipment, second := id(), id()
			if err := all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "2", 667), step(delivery, s.tenant, shipment, s.order, 2),
				step(shipped, s.tenant, shipment, s.order, 1, "1"), bill(s, second, "BD2", s.order, "EUR", 333, "created"),
				step(line, s.tenant, second, 1, s.order, 1, "1", 333))...); err != nil {
				return err
			}
			return shows(tx, s, 6, s.actor, "3 0")
		}, ""},
		{"more billed than is delivered", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "2.000001", 667))...)
		}, "sales_order_item_quantities"},
		{"an item billed in full at less than its net value", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 2, "3", 999))...)
		}, "would be billed 999 of its net value 1000"},
		{"an item billed at more than its net value", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 1001))...)
		}, "would be billed 1001 of its net value 1000"},
		{"a document of a draft order", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), bill(s, s.doc, "BD1", s.other, "EUR", 100, "created"))...)
		}, "its sales order is not a confirmed one"},
		{"a document in another currency than the order's", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), bill(s, s.doc, "BD1", s.order, "USD", 100, "created"))...)
		}, "its sales order is not a confirmed one"},
		{"a document of another customer than the order's", func(tx pgx.Tx, s scene) error {
			o := s
			o.customer = id()
			return all(tx, append(ordered(s), bill(o, s.doc, "BD1", s.order, "EUR", 100, "created"))...)
		}, "its sales order is not a confirmed one"},
		{"a document of another company than the order's", func(tx pgx.Tx, s scene) error {
			o := s
			o.company = id()
			return all(tx, append(ordered(s), bill(o, s.doc, "BD1", s.order, "EUR", 100, "created"))...)
		}, "its sales order is not a confirmed one"},
		{"a document of no order", func(tx pgx.Tx, s scene) error {
			return all(tx, bill(s, s.doc, "BD1", s.order, "EUR", 100, "created"))
		}, "billing_document_sales_order_fkey"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, bill(s, id(), "BD1", s.order, "EUR", 100, "created"))...)
		}, "billing_document_number_once"},
		{"created in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), bill(s, s.doc, "BD1", s.order, "EUR", 100, "cancelled"))...)
		}, "billing_document_status_check"},
		{"a net value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), bill(s, s.doc, "BD1", s.order, "EUR", 0, "created"))...)
		}, "billing_document_net_positive"},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), bill(s, s.doc, "BD1", s.order, "XXA", 100, "created"))...)
		}, "billing_document_currency_fkey"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "0", 100))...)
		}, "billing_document_item_quantity_positive"},
		{"an amount of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 0))...)
		}, "billing_document_item_net_positive"},
		{"an order item twice in a document", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 333), step(line, s.tenant, s.doc, 2, s.order, 1, "1", 334))...)
		}, "billing_document_item_once"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 333), step(line, s.tenant, s.doc, 1, s.order, 2, "1", 333))...)
		}, "billing_document_item_pkey"},
		{"an item the order does not have", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 3, "1", 333))...)
		}, "billing_document_item_order_item_fkey"},
		{"an item of another order than the document's", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.other, 1, "1", 333))...)
		}, "billing_document_item_document_fkey"},
		{"an item of no document", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(line, s.tenant, s.doc, 1, s.order, 1, "1", 333))...)
		}, "billing_document_item_document_fkey"},
		{"a document changed without being posted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`UPDATE sales.billing_document SET version = version + 1`))...)
		}, "only a created document changes, and only by being posted"},
		{"what a document bills changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`UPDATE sales.billing_document SET net_amount = 1`))...)
		}, "permission denied"},
		{"a document deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(`DELETE FROM sales.billing_document`))...)
		}, "permission denied"},
		{"a document's item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 333), step(`UPDATE sales.billing_document_item SET quantity = 2`))...)
		}, "permission denied"},
		{"a document's item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, with(s, step(line, s.tenant, s.doc, 1, s.order, 1, "1", 333), step(`DELETE FROM sales.billing_document_item`))...)
		}, "permission denied"},
		{"what is billed set by the application itself", func(tx pgx.Tx, s scene) error {
			return all(tx, append(ordered(s), step(`UPDATE sales.sales_order_item SET billed_quantity = 1`))...)
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), company: id(), customer: id(), order: id(), other: id(), doc: id()}
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

// TestBillingPostingRules (T79b, migration 000280; ADR-0067 §9): a
// created billing document moves to posted once, naming its customer
// invoice by id and number, its posting date and the invoice's journal
// entry, all of them or none; a customer invoice is one document's; what
// was billed stays as it was.
func TestBillingPostingRules(t *testing.T) {
	need(t)
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', $3, $4, '2026-03-12', 'EUR', 'NT30', 2000, 'draft', $1, $1)`
	const confirm = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	const document = `INSERT INTO sales.billing_document (tenant_id, id, document_number, legal_entity_id, customer_id, sales_order_id, billing_date,
		currency, net_amount, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-03-15', 'EUR', 1000, 'created', $1, $1)`
	const post = `UPDATE sales.billing_document SET status = 'posted', posting_date = $2, customer_invoice_id = $3, customer_invoice_number = $4,
		journal_entry_year = $5, journal_entry_id = $6, version = version + 1, updated_by = tenant_id WHERE id = $1`
	type scene struct{ tenant, company, customer, order, doc, invoice, entry uuid.UUID }
	var none *string
	var noYear *int16
	var noID *uuid.UUID
	year, number, day := int16(2026), "CI100001", "2026-03-16"
	created := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(order, s.tenant, s.order, s.company, s.customer), step(confirm, s.order),
			step(document, s.tenant, s.doc, "BD1", s.company, s.customer, s.order)}, more...)
	}
	posted := func(s scene) stmt { return step(post, s.doc, &day, &s.invoice, &number, &year, &s.entry) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a created document, posted", func(tx pgx.Tx, s scene) error { return all(tx, created(s, posted(s))...) }, ""},
		{"posted without its customer invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(post, s.doc, &day, noID, &number, &year, &s.entry))...)
		}, "billing_document_posted"},
		{"posted without the invoice's number", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(post, s.doc, &day, &s.invoice, none, &year, &s.entry))...)
		}, "billing_document_posted"},
		{"posted without a posting date", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(post, s.doc, none, &s.invoice, &number, &year, &s.entry))...)
		}, "billing_document_posted"},
		{"posted without the invoice's entry", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(post, s.doc, &day, &s.invoice, &number, &year, noID))...)
		}, "billing_document_posted"},
		{"posted without the entry's year", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(post, s.doc, &day, &s.invoice, &number, noYear, &s.entry))...)
		}, "billing_document_posted"},
		{"an invoice number of 41 characters", func(tx pgx.Tx, s scene) error {
			long := strings.Repeat("1", 41)
			return all(tx, created(s, step(post, s.doc, &day, &s.invoice, &long, &year, &s.entry))...)
		}, "billing_document_customer_invoice_number_check"},
		{"a created document given an invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(`UPDATE sales.billing_document SET customer_invoice_id = $1, customer_invoice_number = 'CI1', posting_date = '2026-03-16',
				journal_entry_year = 2026, journal_entry_id = $2, version = version + 1`, s.invoice, s.entry))...)
		}, "only a created document changes, and only by being posted"},
		{"posted twice", func(tx pgx.Tx, s scene) error { return all(tx, created(s, posted(s), posted(s))...) },
			"only a created document changes, and only by being posted"},
		{"a posted document moved back to created", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, posted(s), step(`UPDATE sales.billing_document SET status = 'created', customer_invoice_id = NULL,
				customer_invoice_number = NULL, posting_date = NULL, journal_entry_year = NULL, journal_entry_id = NULL, version = version + 1`))...)
		}, "only a created document changes, and only by being posted"},
		{"posted without moving its version", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(strings.Replace(post, "version = version + 1, ", "", 1), s.doc, &day, &s.invoice, &number, &year, &s.entry))...)
		}, "optimistic lock"},
		{"two documents posted as one customer invoice", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, created(s, step(document, s.tenant, other, "BD2", s.company, s.customer, s.order), posted(s),
				step(post, other, &day, &s.invoice, &number, &year, id()))...)
		}, "billing_document_invoice_once"},
		{"what it bills changed with the posting", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, step(strings.Replace(post, "status = 'posted'", "status = 'posted', net_amount = 1", 1),
				s.doc, &day, &s.invoice, &number, &year, &s.entry))...)
		}, "permission denied"},
		{"a posted document deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, created(s, posted(s), step(`DELETE FROM sales.billing_document`))...)
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), customer: id(), order: id(), doc: id(), invoice: id(), entry: id()}
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
