package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesMaterialItemRules (S1f1, migrations 000395 and 000396): a
// quotation's or a sales order's item that names a material has an item
// key (the material's number); an order item keeps its material, which
// the application may not update.
func TestSalesMaterialItemRules(t *testing.T) {
	need(t)
	const quotation = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'QT1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', '2026-04-12', 'EUR', 100, 'open', $1, $1)`
	const quoted = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, item_key, material_id, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id) VALUES ($1, $2, 1, $3, $4, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid())`
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	const ordered = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, item_key, material_id, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id) VALUES ($1, $2, 1, $3, $4, 'Bracket', 5, 'EA', 20, 1, 100, gen_random_uuid())`
	key, material := "100000001", id()
	cases := []struct {
		name string
		run  func(tenant, doc uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a quotation item of a material with its number", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(quotation, tenant, doc), step(quoted, tenant, doc, key, material)}
		}, ""},
		{"a quotation item of a material without a key", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(quotation, tenant, doc), step(quoted, tenant, doc, nil, material)}
		}, "quotation_item_material_key"},
		{"a quotation item without a material or a key", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(quotation, tenant, doc), step(quoted, tenant, doc, nil, nil)}
		}, ""},
		{"an order item of a material with its number", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(order, tenant, doc), step(ordered, tenant, doc, key, material)}
		}, ""},
		{"an order item of a material without a key", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(order, tenant, doc), step(ordered, tenant, doc, nil, material)}
		}, "sales_order_item_material_key"},
		{"an order item's material changed", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(order, tenant, doc), step(ordered, tenant, doc, key, material),
				step(`UPDATE sales.sales_order_item SET material_id = gen_random_uuid() WHERE sales_order_id = $1`, doc)}
		}, "permission denied"},
		{"an order item's material set", func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(order, tenant, doc), step(ordered, tenant, doc, key, nil),
				step(`UPDATE sales.sales_order_item SET material_id = gen_random_uuid() WHERE sales_order_id = $1`, doc)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, c.run(tenant, doc)...) })
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
