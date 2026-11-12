package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesPriceRules (T80a, migration 000278; ADR-0067 §10): a price
// record is an item key's price above zero per a price unit above zero of
// a known unit of measure in a known currency, for every customer or for
// one, valid from a day until one not before it; it is never changed or
// deleted; a sales item says where its price came from, entered unless
// it says otherwise.
func TestSalesPriceRules(t *testing.T) {
	need(t)
	const record = `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, customer_id, uom_code, currency, unit_price, price_unit,
		valid_from, valid_to, created_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, $7, $8, '2026-03-10', $9, $1)`
	const quotation = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'QT1', $3, gen_random_uuid(), '2026-03-10', '2026-04-10', 'EUR', 100, 'open', $1, $1)`
	const item = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id, price_source) VALUES ($1, $2, 1, 'Bracket', 1, 'EA', 100, 1, 100, gen_random_uuid(), $3)`
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency,
		payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', $3, gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const orderItem = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, net_amount, account_id, price_source) VALUES ($1, $2, 1, 'Bracket', 1, 'EA', 100, 1, 100, gen_random_uuid(), $3)`
	type scene struct{ tenant, company, doc uuid.UUID }
	var open *string
	customer, day := id(), "2026-03-10"
	price := func(s scene, key string, customer *uuid.UUID, unit, currency string, amount int64, priceUnit int, validTo *string) stmt {
		return step(record, s.tenant, s.company, key, customer, unit, currency, amount, priceUnit, validTo)
	}
	recorded := func(s scene) stmt { return price(s, "BRACKET-M8", nil, "EA", "EUR", 1250, 1, open) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a general price and a customer's, one with a last day", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), price(s, "BRACKET-M8", &customer, "EA", "EUR", 117500, 100, &day))
		}, ""},
		{"the same price recorded again", func(tx pgx.Tx, s scene) error { return all(tx, recorded(s), recorded(s)) }, ""},
		{"valid until the day before it starts", func(tx pgx.Tx, s scene) error {
			before := "2026-03-09"
			return all(tx, price(s, "BRACKET-M8", nil, "EA", "EUR", 1250, 1, &before))
		}, "price_validity"},
		{"a price of zero", func(tx pgx.Tx, s scene) error { return all(tx, price(s, "BRACKET-M8", nil, "EA", "EUR", 0, 1, open)) }, "price_positive"},
		{"a price unit of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, "BRACKET-M8", nil, "EA", "EUR", 1250, 0, open))
		}, "price_unit_positive"},
		{"an item key of white space", func(tx pgx.Tx, s scene) error { return all(tx, price(s, "  ", nil, "EA", "EUR", 1250, 1, open)) }, "price_item_key_check"},
		{"an item key of 41 characters", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, strings.Repeat("K", 41), nil, "EA", "EUR", 1250, 1, open))
		}, "price_item_key_check"},
		{"an unknown unit", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, "BRACKET-M8", nil, "XX", "EUR", 1250, 1, open))
		}, "price_uom_code_fkey"},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, "BRACKET-M8", nil, "EA", "XXA", 1250, 1, open))
		}, "price_currency_fkey"},
		{"a record changed", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), step(`UPDATE sales.price SET unit_price = 1`))
		}, "permission denied"},
		{"a record deleted", func(tx pgx.Tx, s scene) error { return all(tx, recorded(s), step(`DELETE FROM sales.price`)) }, "permission denied"},
		{"a quotation item priced from a record", func(tx pgx.Tx, s scene) error {
			return all(tx, step(quotation, s.tenant, s.doc, s.company), step(item, s.tenant, s.doc, "price_list"))
		}, ""},
		{"a quotation item with an unknown price source", func(tx pgx.Tx, s scene) error {
			return all(tx, step(quotation, s.tenant, s.doc, s.company), step(item, s.tenant, s.doc, "catalog"))
		}, "quotation_item_price_source_check"},
		{"an order item priced from a record", func(tx pgx.Tx, s scene) error {
			return all(tx, step(order, s.tenant, s.doc, s.company), step(orderItem, s.tenant, s.doc, "price_list"))
		}, ""},
		{"an order item with an unknown price source", func(tx pgx.Tx, s scene) error {
			return all(tx, step(order, s.tenant, s.doc, s.company), step(orderItem, s.tenant, s.doc, "catalog"))
		}, "sales_order_item_price_source_check"},
		{"an item that names no price source has an entered price", func(tx pgx.Tx, s scene) error {
			unnamed := strings.Replace(strings.Replace(item, ", price_source)", ")", 1), ", $3)", ")", 1)
			if err := all(tx, step(quotation, s.tenant, s.doc, s.company), step(unnamed, s.tenant, s.doc)); err != nil {
				return err
			}
			return exec(tx, `DO $$ BEGIN IF EXISTS (SELECT FROM sales.quotation_item WHERE price_source <> 'manual') THEN
				RAISE EXCEPTION 'the default price source is not manual'; END IF; END $$`)
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), doc: id()}
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
