package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesConditionRules (T80b, migration 000295; ADR-0067 §11): a
// price record is a price (a unit, a currency, a price per a price unit)
// or a discount of a condition type (a percent more than 0 and less than
// 100), never a mix; a condition type is a short code; a sales item's
// conditions are a JSON array, empty unless it says otherwise.
func TestSalesConditionRules(t *testing.T) {
	need(t)
	const record = `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, condition_type, uom_code, currency, unit_price, price_unit,
		percent, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, 'BRACKET-M8', $3, $4, $5, $6, $7, $8::numeric, '2026-03-10', $1)`
	const quotation = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'QT1', $3, gen_random_uuid(), '2026-03-10', '2026-04-10', 'EUR', 100, 'open', $1, $1)`
	const item = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id, conditions) VALUES ($1, $2, $3, 'Bracket', 1, 'EA', 100, 1, 100, gen_random_uuid(), $4::jsonb)`
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date, currency,
		payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', $3, gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const orderItem = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, net_amount, account_id, conditions) VALUES ($1, $2, 1, 'Bracket', 1, 'EA', 100, 1, 100, gen_random_uuid(), $3::jsonb)`
	type scene struct{ tenant, company, doc uuid.UUID }
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	var none *string
	var no *int64
	price := func(s scene, condition *string) stmt {
		return step(record, s.tenant, s.company, condition, "EA", "EUR", num(1250), num(1), none)
	}
	discount := func(s scene, condition, percent *string) stmt {
		return step(record, s.tenant, s.company, condition, none, none, no, no, percent)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a price without a type, a price of a type and a discount", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, nil), price(s, str("PR00")), discount(s, str("K007"), str("5.5")))
		}, ""},
		{"the largest discount", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("K007"), str("99.999999"))) }, ""},
		{"a discount of nothing", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("K007"), str("0"))) }, "price_percent_range"},
		{"a discount of everything", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("K007"), str("100"))) }, "price_percent_range"},
		{"a discount without a type", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, nil, str("5"))) }, "price_or_percent"},
		{"a discount with a unit", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, "K007", "EA", none, no, no, "5"))
		}, "price_or_percent"},
		{"a discount with a currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, "K007", none, "EUR", no, no, "5"))
		}, "price_or_percent"},
		{"a discount with a price", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, "K007", none, none, num(1250), no, "5"))
		}, "price_or_percent"},
		{"a discount with a price unit", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, "K007", none, none, no, num(1), "5"))
		}, "price_or_percent"},
		{"a price with a percent", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, "PR00", "EA", "EUR", num(1250), num(1), "5"))
		}, "price_or_percent"},
		{"a price without a unit", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, none, none, "EUR", num(1250), num(1), none))
		}, "price_or_percent"},
		{"a price without a currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, none, "EA", none, num(1250), num(1), none))
		}, "price_or_percent"},
		{"a price without a price", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, none, "EA", "EUR", no, num(1), none))
		}, "price_or_percent"},
		{"a price without a price unit", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, none, "EA", "EUR", num(1250), no, none))
		}, "price_or_percent"},
		{"a condition type in small letters", func(tx pgx.Tx, s scene) error { return all(tx, price(s, str("pr00"))) }, "price_condition_type_check"},
		{"a condition type of eleven characters", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, str("PR000000000")))
		}, "price_condition_type_check"},
		{"a quotation item with its conditions and one without", func(tx pgx.Tx, s scene) error {
			if err := all(tx, step(quotation, s.tenant, s.doc, s.company),
				step(item, s.tenant, s.doc, 1, `[{"condition_type": "PR00", "name": "Price", "kind": "price", "unit_price": 100, "price_unit": 1, "amount": 100}]`),
				step(`INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code, unit_price, price_unit,
					net_amount, account_id) VALUES ($1, $2, 2, 'Bracket', 1, 'EA', 100, 1, 100, gen_random_uuid())`, s.tenant, s.doc)); err != nil {
				return err
			}
			return exec(tx, `DO $$ BEGIN IF (SELECT conditions FROM sales.quotation_item WHERE item_no = 2) <> '[]'::jsonb THEN
				RAISE EXCEPTION 'the default conditions are not an empty array'; END IF; END $$`)
		}, ""},
		{"a quotation item whose conditions are an object", func(tx pgx.Tx, s scene) error {
			return all(tx, step(quotation, s.tenant, s.doc, s.company), step(item, s.tenant, s.doc, 1, `{"condition_type": "PR00"}`))
		}, "quotation_item_conditions_array"},
		{"a quotation item whose conditions are JSON null", func(tx pgx.Tx, s scene) error {
			return all(tx, step(quotation, s.tenant, s.doc, s.company), step(item, s.tenant, s.doc, 1, `null`))
		}, "quotation_item_conditions_array"},
		{"an order item with its conditions", func(tx pgx.Tx, s scene) error {
			return all(tx, step(order, s.tenant, s.doc, s.company), step(orderItem, s.tenant, s.doc, `[]`))
		}, ""},
		{"an order item whose conditions are a text", func(tx pgx.Tx, s scene) error {
			return all(tx, step(order, s.tenant, s.doc, s.company), step(orderItem, s.tenant, s.doc, `"PR00"`))
		}, "sales_order_item_conditions_array"},
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
