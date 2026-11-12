package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRevenuePermissions (T81a, migration 000280; ADR-0068 §6): the
// revenue permissions are seeded and are no approval permissions.
func TestRevenuePermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'revenue.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "revenue.contract.read revenue.contract.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestRevenueContractRules (T81a, migration 000280; ADR-0068 §3, §4): a
// revenue contract is created as a draft with a transaction price above
// zero in a known currency, one per sales order; an obligation is one per
// order item, numbered from 1, described, with a net value and a
// stand-alone selling price above zero and a known timing; the
// application changes and deletes neither.
func TestRevenueContractRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
		transaction_price, status, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), $3, $4, 'EUR', $5, $6, $1, $1)`
	const obligation = `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
		timing, standalone_selling_price) VALUES ($1, $2, $3, $4, 'Bracket', $5, $6, $7)`
	type scene struct{ tenant, doc, order uuid.UUID }
	contract := func(s scene, doc, order uuid.UUID, number string, price int64, status string) stmt {
		return step(header, s.tenant, doc, order, number, price, status)
	}
	made := func(s scene) stmt { return contract(s, s.doc, s.order, "SO1", 270000, "draft") }
	promised := func(s scene, no, item int, net int64, timing string, price int64) stmt {
		return step(obligation, s.tenant, s.doc, no, item, net, timing, price)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a draft with its obligations", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 150000, "point_in_time", 150000), promised(s, 2, 2, 120000, "over_time", 130000))
		}, ""},
		{"a second contract of the sales order", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), contract(s, id(), s.order, "SO1", 270000, "draft"))
		}, "contract_one_per_sales_order"},
		{"a second contract of the sales order, left as it is", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(header+` ON CONFLICT ON CONSTRAINT contract_one_per_sales_order DO NOTHING`, s.tenant, id(), s.order, "SO1", 1, "draft"),
				step(`DO $$ BEGIN IF (SELECT count(*) FROM revenue.contract) <> 1 THEN RAISE EXCEPTION 'two contracts of one order'; END IF; END $$`))
		}, ""},
		{"another order's contract", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), contract(s, id(), id(), "SO2", 270000, "draft"))
		}, ""},
		{"created in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, contract(s, s.doc, s.order, "SO1", 270000, "open"))
		}, "contract_status_check"},
		{"a transaction price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, contract(s, s.doc, s.order, "SO1", 0, "draft"))
		}, "contract_transaction_price_positive"},
		{"without the order's number", func(tx pgx.Tx, s scene) error {
			return all(tx, contract(s, s.doc, s.order, "", 270000, "draft"))
		}, "contract_sales_order_number_check"},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(strings.Replace(header, "'EUR'", "'XXA'", 1), s.tenant, s.doc, s.order, "SO1", 270000, "draft"))
		}, "contract_currency_fkey"},
		{"a contract changed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(`UPDATE revenue.contract SET transaction_price = 1`))
		}, "permission denied"},
		{"a contract deleted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(`DELETE FROM revenue.contract`)) }, "permission denied"},
		{"two obligations of one order item", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "point_in_time", 100), promised(s, 2, 1, 100, "point_in_time", 100))
		}, "performance_obligation_one_per_order_item"},
		{"an obligation numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "point_in_time", 100), promised(s, 1, 2, 100, "point_in_time", 100))
		}, "performance_obligation_pkey"},
		{"an obligation numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 0, 1, 100, "point_in_time", 100))
		}, "performance_obligation_obligation_no_check"},
		{"an order item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 0, 100, "point_in_time", 100))
		}, "performance_obligation_order_item_no_check"},
		{"an order item's value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 0, "point_in_time", 100))
		}, "performance_obligation_net_positive"},
		{"a stand-alone selling price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "point_in_time", 0))
		}, "performance_obligation_price_positive"},
		{"an unknown timing", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "on_delivery", 100))
		}, "performance_obligation_timing_check"},
		{"a description of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(strings.Replace(obligation, "'Bracket'", "'  '", 1), s.tenant, s.doc, 1, 1, 100, "point_in_time", 100))
		}, "performance_obligation_description_check"},
		{"an item key of white space", func(tx pgx.Tx, s scene) error {
			keyed := strings.Replace(strings.Replace(obligation, "description,", "description, item_key,", 1), "'Bracket',", "'Bracket', ' ',", 1)
			return all(tx, made(s), step(keyed, s.tenant, s.doc, 1, 1, 100, "point_in_time", 100))
		}, "performance_obligation_item_key_check"},
		{"an obligation of no contract", func(tx pgx.Tx, s scene) error {
			return all(tx, promised(s, 1, 1, 100, "point_in_time", 100))
		}, "performance_obligation_tenant_id_contract_id_fkey"},
		{"an obligation changed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "point_in_time", 100), step(`UPDATE revenue.performance_obligation SET standalone_selling_price = 200`))
		}, "permission denied"},
		{"an obligation deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), promised(s, 1, 1, 100, "point_in_time", 100), step(`DELETE FROM revenue.performance_obligation`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), doc: id(), order: id()}
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

// TestRevenueInbox (T81a, migration 000280; ADR-0068 §4; ADR-0007): the
// consumer records an event once: a second record of it changes nothing.
func TestRevenueInbox(t *testing.T) {
	need(t)
	tenant, event := id(), id()
	const read = `INSERT INTO revenue.inbox (tenant_id, consumer, event_id) VALUES ($1, 'revenue-contracts', $2)`
	err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, step(read, tenant, event), step(read, tenant, event)) })
	failsWith(t, err, "inbox_pkey")
	err = asApp(t, tenant, func(tx pgx.Tx) error {
		return all(tx, step(read, tenant, event), step(read+` ON CONFLICT DO NOTHING`, tenant, event))
	})
	if err != nil {
		t.Fatalf("a second record left as it is: got %v, want no error", err)
	}
}
