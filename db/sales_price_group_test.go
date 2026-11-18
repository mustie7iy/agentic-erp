package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesPriceGroupRules (T80f, migration 000374; ADR-0067 §13): a
// customer has one sales data per company, with its number and a price
// group that is a short code or none; a change sets the group, the
// version and who changed it, and nothing else, and no sales data is
// deleted; a price record is for one customer or for a price group,
// never both, and a group is a short code.
func TestSalesPriceGroupRules(t *testing.T) {
	need(t)
	const data = `INSERT INTO sales.customer_sales_data (tenant_id, id, legal_entity_id, customer_id, customer_number, price_group, version,
		created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, $1, $1)`
	const record = `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, customer_id, price_group, uom_code, currency, unit_price,
		price_unit, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, 'BRACKET-M8', $3, $4, 'EA', 'EUR', 1250, 1, '2026-03-10', $1)`
	type scene struct{ tenant, company, customer uuid.UUID }
	str := func(s string) *string { return &s }
	var none *string
	var nobody *uuid.UUID
	salesData := func(s scene, number string, group *string, version int) stmt {
		return step(data, s.tenant, s.company, s.customer, number, group, version)
	}
	wholesale := func(s scene) stmt { return salesData(s, "BP1", str("01"), 1) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"sales data with a group and without one", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), step(data, s.tenant, s.company, id(), "BP2", none, 1))
		}, ""},
		{"a customer number of nothing", func(tx pgx.Tx, s scene) error { return all(tx, salesData(s, "", none, 1)) },
			"customer_sales_data_customer_number_check"},
		{"a customer number of 41 characters", func(tx pgx.Tx, s scene) error { return all(tx, salesData(s, strings.Repeat("9", 41), none, 1)) },
			"customer_sales_data_customer_number_check"},
		{"a group in small letters", func(tx pgx.Tx, s scene) error { return all(tx, salesData(s, "BP1", str("w1"), 1)) },
			"customer_sales_data_price_group_check"},
		{"a group of eleven characters", func(tx pgx.Tx, s scene) error { return all(tx, salesData(s, "BP1", str("01234567890"), 1)) },
			"customer_sales_data_price_group_check"},
		{"version 0", func(tx pgx.Tx, s scene) error { return all(tx, salesData(s, "BP1", none, 0)) }, "customer_sales_data_version_check"},
		{"a second sales data of the customer in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), salesData(s, "BP1", str("02"), 1))
		}, "customer_sales_data_once"},
		{"the group changed", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), step(`UPDATE sales.customer_sales_data SET price_group = '02', version = version + 1, updated_by = $1`, s.tenant))
		}, ""},
		{"the customer changed", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), step(`UPDATE sales.customer_sales_data SET customer_id = $1`, id()))
		}, "permission denied"},
		{"the company changed", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), step(`UPDATE sales.customer_sales_data SET legal_entity_id = $1`, id()))
		}, "permission denied"},
		{"sales data deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, wholesale(s), step(`DELETE FROM sales.customer_sales_data`))
		}, "permission denied"},
		{"a record for a group", func(tx pgx.Tx, s scene) error { return all(tx, step(record, s.tenant, s.company, nobody, "01")) }, ""},
		{"a record for a customer and a group", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, s.customer, "01"))
		}, "price_customer_or_group"},
		{"a record for a group in small letters", func(tx pgx.Tx, s scene) error {
			return all(tx, step(record, s.tenant, s.company, nobody, "w1"))
		}, "price_price_group_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), customer: id()}
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
