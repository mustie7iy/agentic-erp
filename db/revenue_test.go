package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRevenuePermissions (T81a, migration 000280; T81b, migration 000281;
// ADR-0068 §6): the
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
	if got, want := strings.Join(codes, " "), "revenue.contract.activate revenue.contract.read revenue.contract.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestRevenueContractRules (T81a, migration 000280; ADR-0068 §3, §4): a
// revenue contract is created as a draft with a transaction price above
// zero in a known currency, one per sales order; an obligation is one per
// order item, numbered from 1, described, with a net value and a
// stand-alone selling price above zero and a known timing; nothing is
// deleted (what changes, and when, is TestRevenueAllocationRules').
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
			return all(tx, made(s), promised(s, 1, 1, 150000, "point_in_time", 150000), promised(s, 2, 2, 120000, "point_in_time", 130000))
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
		}, "performance_obligation_service_period"}, // which names the two timings too, and is checked before performance_obligation_timing_check
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

// TestRevenueAllocationRules (T81b, migration 000281; ADR-0068 §7): while
// a contract is a draft its obligations' timing, service period and
// stand-alone selling price change, and its version with them; an
// over-time obligation has a service period, from not after to, and a
// point-in-time obligation none; activating is a draft's last change and
// needs every obligation's allocated amount, which add up to the
// transaction price; an active contract and its obligations are fixed,
// and nothing else of either is the application's to change.
func TestRevenueAllocationRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
		transaction_price, status, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'SO1', 'EUR', 270000, 'draft', $1, $1)`
	const obligation = `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
		timing, standalone_selling_price) VALUES ($1, $2, $3, $3, 'Bracket', $4, 'point_in_time', $4)`
	const period = `UPDATE revenue.performance_obligation SET timing = $2, service_from = $3, service_to = $4 WHERE obligation_no = $1`
	const share = `UPDATE revenue.performance_obligation SET allocated_amount = $2 WHERE obligation_no = $1`
	const status = `UPDATE revenue.contract SET status = $1, version = version + 1, updated_by = tenant_id`
	type scene struct{ tenant, doc uuid.UUID }
	var none *string
	day := func(s string) *string { return &s }
	made := func(s scene) []stmt {
		return []stmt{step(header, s.tenant, s.doc), step(obligation, s.tenant, s.doc, 1, 150000), step(obligation, s.tenant, s.doc, 2, 120000)}
	}
	with := func(s scene, more ...stmt) []stmt { return append(made(s), more...) }
	active := func(s scene, more ...stmt) []stmt {
		return append(with(s, step(share, 1, 144643), step(share, 2, 125357), step(status, "active")), more...)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"an obligation set to over time with its period and price", func(s scene) []stmt {
			return with(s, step(period, 2, "over_time", day("2026-01-01"), day("2026-12-31")),
				step(`UPDATE revenue.performance_obligation SET standalone_selling_price = 130000 WHERE obligation_no = 2`), step(status, "draft"))
		}, ""},
		{"a service period of one day", func(s scene) []stmt {
			return with(s, step(period, 2, "over_time", day("2026-06-30"), day("2026-06-30")))
		}, ""},
		{"over time without a service period", func(s scene) []stmt { return with(s, step(period, 2, "over_time", none, none)) },
			"performance_obligation_service_period"},
		{"over time without the period's end", func(s scene) []stmt {
			return with(s, step(period, 2, "over_time", day("2026-01-01"), none))
		}, "performance_obligation_service_period"},
		{"a period that ends before it starts", func(s scene) []stmt {
			return with(s, step(period, 2, "over_time", day("2026-02-01"), day("2026-01-31")))
		}, "performance_obligation_service_period"},
		{"a service period at a point in time", func(s scene) []stmt {
			return with(s, step(period, 2, "point_in_time", day("2026-01-01"), day("2026-12-31")))
		}, "performance_obligation_service_period"},
		{"a negative allocated amount", func(s scene) []stmt { return with(s, step(share, 1, -1)) }, "performance_obligation_allocated_check"},
		{"activated with shares that are the transaction price", func(s scene) []stmt { return active(s) }, ""},
		{"activated with a share of nothing", func(s scene) []stmt {
			return with(s, step(share, 1, 270000), step(share, 2, 0), step(status, "active"))
		}, ""},
		{"activated without the shares", func(s scene) []stmt { return with(s, step(status, "active")) },
			"allocated amounts are not its transaction price"},
		{"activated with one share missing", func(s scene) []stmt {
			return with(s, step(share, 1, 270000), step(status, "active"))
		}, "allocated amounts are not its transaction price"},
		{"activated with shares a minor unit short", func(s scene) []stmt {
			return with(s, step(share, 1, 144642), step(share, 2, 125357), step(status, "active"))
		}, "allocated amounts are not its transaction price"},
		{"activated with shares a minor unit over", func(s scene) []stmt {
			return with(s, step(share, 1, 144643), step(share, 2, 125358), step(status, "active"))
		}, "allocated amounts are not its transaction price"},
		{"activated without obligations", func(s scene) []stmt {
			return []stmt{step(header, s.tenant, s.doc), step(status, "active")}
		}, "allocated amounts are not its transaction price"},
		{"an unknown status", func(s scene) []stmt { return with(s, step(status, "cancelled")) }, "contract_status_check"},
		{"activated twice", func(s scene) []stmt { return active(s, step(status, "active")) }, "only a draft changes"},
		{"an active contract back to a draft", func(s scene) []stmt { return active(s, step(status, "draft")) }, "only a draft changes"},
		{"an active contract's obligation changed", func(s scene) []stmt {
			return active(s, step(`UPDATE revenue.performance_obligation SET standalone_selling_price = 1 WHERE obligation_no = 1`))
		}, "an active contract's obligations are fixed"},
		{"an active contract's share changed", func(s scene) []stmt { return active(s, step(share, 1, 144642)) },
			"an active contract's obligations are fixed"},
		{"an obligation added to an active contract", func(s scene) []stmt {
			return active(s, step(obligation, s.tenant, s.doc, 3, 100))
		}, "an active contract's obligations are fixed"},
		{"a contract's transaction price changed", func(s scene) []stmt {
			return with(s, step(`UPDATE revenue.contract SET transaction_price = 1`))
		}, "permission denied"},
		{"an obligation's order value changed", func(s scene) []stmt {
			return with(s, step(`UPDATE revenue.performance_obligation SET net_amount = 1`))
		}, "permission denied"},
		{"an obligation moved to another order item", func(s scene) []stmt {
			return with(s, step(`UPDATE revenue.performance_obligation SET order_item_no = 9`))
		}, "permission denied"},
		{"an obligation deleted", func(s scene) []stmt { return with(s, step(`DELETE FROM revenue.performance_obligation`)) }, "permission denied"},
		{"a contract deleted", func(s scene) []stmt {
			return []stmt{step(header, s.tenant, s.doc), step(`DELETE FROM revenue.contract`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), doc: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestRevenueAllocationRevert (T81b, migration 000281's down): the revert
// is refused, before it changes anything, while an over-time obligation
// exists, whose service period it would drop.
func TestRevenueAllocationRevert(t *testing.T) {
	need(t)
	down := ""
	for _, m := range Migrations() {
		if strings.HasSuffix(m.Name, "_revenue_allocation") {
			down = m.Down
		}
	}
	if down == "" {
		t.Fatal("got no migration named …_revenue_allocation in db/migrations, want the one the test is about")
	}
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, doc := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
		step(`INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
			transaction_price, status, created_by, updated_by)
			VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'SO1', 'EUR', 100, 'draft', $1, $1)`, tenant, doc),
		step(`INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
			timing, service_from, service_to, standalone_selling_price)
			VALUES ($1, $2, 1, 1, 'Support', 100, 'over_time', '2026-01-01', '2026-12-31', 100)`, tenant, doc)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would drop their service periods")
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
