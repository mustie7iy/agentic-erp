package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesItemJurisdictionRules (migration 000420; ADR-0067 As built, the
// emirate on a sales item): a quotation's or a sales order's item names
// the jurisdiction its tax is levied in only under a tax code, in the form
// of an ISO 3166-2 subdivision; an order item keeps it, which the
// application may not update.
func TestSalesItemJurisdictionRules(t *testing.T) {
	need(t)
	const quotation = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'QT1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', '2026-04-12', 'AED', 100, 'open', $1, $1)`
	const quoted = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id, tax_code, tax_jurisdiction) VALUES ($1, $2, 1, 'Chairs', 5, 'EA', 20, 1, 100, gen_random_uuid(), $3, $4)`
	const order = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'AED', 'NT30', 100, 'draft', $1, $1)`
	const ordered = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code,
		unit_price, price_unit, net_amount, account_id, tax_code, tax_jurisdiction) VALUES ($1, $2, 1, 'Chairs', 5, 'EA', 20, 1, 100, gen_random_uuid(), $3, $4)`
	var none *string
	code, dubai, country, name := "S5", "AE-DU", "AE", "Dubai"
	quote := func(code, jurisdiction *string) func(tenant, doc uuid.UUID) []stmt {
		return func(tenant, doc uuid.UUID) []stmt {
			return []stmt{step(quotation, tenant, doc), step(quoted, tenant, doc, code, jurisdiction)}
		}
	}
	place := func(code, jurisdiction *string, more ...string) func(tenant, doc uuid.UUID) []stmt {
		return func(tenant, doc uuid.UUID) []stmt {
			out := []stmt{step(order, tenant, doc), step(ordered, tenant, doc, code, jurisdiction)}
			for _, m := range more {
				out = append(out, step(m, doc))
			}
			return out
		}
	}
	cases := []struct {
		name string
		run  func(tenant, doc uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a quotation item's emirate under a tax code", quote(&code, &dubai), ""},
		{"a quotation item's tax code without a jurisdiction", quote(&code, none), ""},
		{"a quotation item's jurisdiction without a tax code", quote(none, &dubai), "quotation_item_tax_jurisdiction"},
		{"a quotation item's country, not a subdivision", quote(&code, &country), "quotation_item_tax_jurisdiction"},
		{"a quotation item's name, not a code", quote(&code, &name), "quotation_item_tax_jurisdiction"},
		{"an order item's emirate under a tax code", place(&code, &dubai), ""},
		{"an order item without either", place(none, none), ""},
		{"an order item's jurisdiction without a tax code", place(none, &dubai), "sales_order_item_tax_jurisdiction"},
		{"an order item's country, not a subdivision", place(&code, &country), "sales_order_item_tax_jurisdiction"},
		{"an order item's name, not a code", place(&code, &name), "sales_order_item_tax_jurisdiction"},
		{"an order item's jurisdiction changed", place(&code, &dubai,
			`UPDATE sales.sales_order_item SET tax_jurisdiction = 'AE-AZ' WHERE sales_order_id = $1`), "permission denied"},
		{"an order item's jurisdiction set", place(&code, none,
			`UPDATE sales.sales_order_item SET tax_jurisdiction = 'AE-DU' WHERE sales_order_id = $1`), "permission denied"},
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
