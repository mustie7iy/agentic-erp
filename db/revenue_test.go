package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRevenuePermissions (T81a, migration 000298; T81b, migration 000299;
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
	if got, want := strings.Join(codes, " "), "revenue.contract.activate revenue.contract.read revenue.contract.write revenue.recognition.run"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestRevenueContractRules (T81a, migration 000298; ADR-0068 §3, §4): a
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

// TestRevenueAllocationRules (T81b, migration 000299; ADR-0068 §7): while
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

// TestRevenueAllocationRevert (T81b, migration 000299's down): the revert
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

// TestRevenueSatisfiedOn (T81c, migration 000303; ADR-0068 §8): while a
// contract is a draft a point-in-time obligation may name the day it is
// satisfied on; an over-time obligation may not; an active contract's
// obligation keeps what it has; the revert is refused, before it changes
// anything, while an obligation names such a day.
func TestRevenueSatisfiedOn(t *testing.T) {
	need(t)
	const header = `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
		transaction_price, status, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'SO1', 'EUR', 100, 'draft', $1, $1)`
	const obligation = `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
		timing, standalone_selling_price) VALUES ($1, $2, 1, 1, 'Bracket', 100, 'point_in_time', 100)`
	const on = `UPDATE revenue.performance_obligation SET satisfied_on = $1`
	const activate = `UPDATE revenue.performance_obligation SET allocated_amount = 100`
	const active = `UPDATE revenue.contract SET status = 'active', version = version + 1, updated_by = tenant_id`
	var none *string
	day := "2026-04-10"
	cases := []struct {
		name string
		more []stmt
		want string // empty: allowed
	}{
		{"a point-in-time obligation's day", []stmt{step(on, &day)}, ""},
		{"the day removed", []stmt{step(on, &day), step(on, none)}, ""},
		{"an over-time obligation's day", []stmt{step(on, &day),
			step(`UPDATE revenue.performance_obligation SET timing = 'over_time', service_from = '2026-01-01', service_to = '2026-12-31'`)},
			"performance_obligation_satisfied_on"},
		{"a day set on an over-time obligation", []stmt{
			step(`UPDATE revenue.performance_obligation SET timing = 'over_time', service_from = '2026-01-01', service_to = '2026-12-31'`), step(on, &day)},
			"performance_obligation_satisfied_on"},
		{"an active contract's obligation keeps its day", []stmt{step(on, &day), step(activate), step(active), step(on, none)},
			"an active contract's obligations are fixed"},
		{"an active contract's obligation takes no day", []stmt{step(activate), step(active), step(on, &day)},
			"an active contract's obligations are fixed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{step(header, tenant, doc), step(obligation, tenant, doc)}, c.more...)...)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("the revert", func(t *testing.T) {
		down := ""
		for _, m := range Migrations() {
			if strings.HasSuffix(m.Name, "_revenue_satisfied_on") {
				down = m.Down
			}
		}
		if down == "" {
			t.Fatal("got no migration named …_revenue_satisfied_on in db/migrations, want the one the test is about")
		}
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		tenant, doc := id(), id()
		if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(header, tenant, doc), step(obligation, tenant, doc),
			step(on+` WHERE tenant_id = $2`, &day, tenant)); err != nil { // the owner's transaction reads every tenant's rows
			t.Fatal(err)
		}
		failsWith(t, exec(tx, down), "reverting would drop it")
	})
}

// TestRevenueInbox (T81a, migration 000298; ADR-0068 §4; ADR-0007): the
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

// TestRevenueRecognitionRules (T81d, migrations 000304 and 000305;
// ADR-0068 §9): one run per company and period, each for a later period
// than the company's last; a run's positions are what it recognized less
// what it billed, and one that posted nothing moved no revenue; a posting is of a run and a contract, once, and its
// obligations are the contract's; runs and postings are written once;
// the two account determination keys and the journal's source context
// are taken, their checks validated; the revert is refused while a run
// exists.
func TestRevenueRecognitionRules(t *testing.T) {
	need(t)
	const run = `INSERT INTO revenue.recognition_run (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, posting_date, currency, contract_count,
		posting_count, recognized_amount, billed_amount, contract_asset_amount, contract_liability_amount, revenue_amount, created_by)
		VALUES ($1, $2, $3, 2026, $4, $5, 'EUR', $6, $7, $8, $9, $10, $11, $12, $1)`
	const contract = `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
		transaction_price, status, created_by, updated_by) VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), 'SO1', 'EUR', 100, 'draft', $1, $1)`
	const obligation = `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
		timing, standalone_selling_price) VALUES ($1, $2, 1, 1, 'Bracket', 100, 'point_in_time', 100)`
	const posting = `INSERT INTO revenue.recognition_posting (tenant_id, run_id, contract_id, id, recognized_amount, billed_amount, revenue_amount,
		journal_entry_id, journal_entry_year, document_number) VALUES ($1, $2, $3, gen_random_uuid(), 100, 0, 100, gen_random_uuid(), 2026, '100001')`
	const figures = `INSERT INTO revenue.recognition_posting_obligation (tenant_id, run_id, contract_id, obligation_no, account_id, recognized_amount,
		billed_amount, revenue_amount) VALUES ($1, $2, $3, $4, gen_random_uuid(), 100, 0, 100)`
	type scene struct{ tenant, le, run, doc uuid.UUID }
	february := func(s scene) stmt {
		return step(run, s.tenant, s.run, s.le, 2, "2026-02-28", 1, 1, 100, 0, 100, 0, 100)
	}
	other := func(s scene, period int, day string) stmt {
		return step(run, s.tenant, id(), s.le, period, day, 0, 0, 0, 0, 0, 0, 0)
	}
	posted := func(s scene, more ...stmt) []stmt {
		return append([]stmt{february(s), step(contract, s.tenant, s.doc, s.le), step(obligation, s.tenant, s.doc), step(posting, s.tenant, s.run, s.doc),
			step(figures, s.tenant, s.run, s.doc, 1)}, more...)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a run with a posting and its obligation", func(s scene) []stmt { return posted(s) }, ""},
		{"a later period's run", func(s scene) []stmt { return []stmt{february(s), other(s, 3, "2026-03-31")} }, ""},
		{"another company's run for an earlier period", func(s scene) []stmt {
			return []stmt{february(s), step(run, s.tenant, id(), id(), 1, "2026-01-31", 0, 0, 0, 0, 0, 0, 0)}
		}, ""},
		{"a second run of the period", func(s scene) []stmt { return []stmt{february(s), other(s, 2, "2026-02-28")} }, "recognition_run_once"},
		{"a run for an earlier period", func(s scene) []stmt { return []stmt{february(s), other(s, 1, "2026-01-31")} }, "the company has a run for this day or a later one"},
		{"positions that are not recognized less billed", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.le, 2, "2026-02-28", 1, 1, 100, 0, 90, 0, 100)}
		}, "recognition_run_positions_check"},
		{"more postings than contracts", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.le, 2, "2026-02-28", 1, 2, 0, 0, 0, 0, 0)}
		}, "recognition_run_posting_count_check"},
		{"a negative contract asset", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.le, 2, "2026-02-28", 1, 1, 0, 0, -10, -10, 0)}
		}, "recognition_run_asset_check"},
		{"revenue moved by a run that posted nothing", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.le, 2, "2026-02-28", 1, 0, 0, 0, 0, 0, 100)}
		}, "recognition_run_revenue_check"},
		{"a contract posted twice in a run", func(s scene) []stmt { return posted(s, step(posting, s.tenant, s.run, s.doc)) }, "recognition_posting_pkey"},
		{"a posting of no run", func(s scene) []stmt {
			return []stmt{step(contract, s.tenant, s.doc, s.le), step(posting, s.tenant, s.run, s.doc)}
		}, "recognition_posting_tenant_id_run_id_fkey"},
		{"an obligation the contract does not have", func(s scene) []stmt { return posted(s, step(figures, s.tenant, s.run, s.doc, 2)) },
			"recognition_posting_obligatio_tenant_id_contract_id_obliga_fkey"},
		{"a run changed", func(s scene) []stmt {
			return []stmt{february(s), step(`UPDATE revenue.recognition_run SET posting_count = 0`)}
		}, "permission denied"},
		{"a run deleted", func(s scene) []stmt { return []stmt{february(s), step(`DELETE FROM revenue.recognition_run`)} }, "permission denied"},
		{"a posting changed", func(s scene) []stmt {
			return posted(s, step(`UPDATE revenue.recognition_posting SET revenue_amount = 0`))
		}, "permission denied"},
		{"a posting's obligation deleted", func(s scene) []stmt { return posted(s, step(`DELETE FROM revenue.recognition_posting_obligation`)) },
			"permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), le: id(), run: id(), doc: id()}
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
	t.Run("the widened checks are validated", func(t *testing.T) {
		var tables, checked int
		var keys bool
		if err := testDB.QueryRow(t.Context(), `SELECT (SELECT count(*) + 1 FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass),
				(SELECT count(*) FROM pg_constraint WHERE conname = 'journal_entry_source_context_check' AND convalidated
					AND pg_get_constraintdef(oid) LIKE '%''revenue''%'),
				(SELECT convalidated AND pg_get_constraintdef(oid) LIKE '%''contract_asset''%' AND pg_get_constraintdef(oid) LIKE '%''contract_liability''%'
					FROM pg_constraint WHERE conname = 'account_determination_key_check')`).Scan(&tables, &checked, &keys); err != nil {
			t.Fatal(err)
		}
		if checked != tables {
			t.Errorf("validated source context checks that take revenue: got %d, want %d (the table and its partitions)", checked, tables)
		}
		if !keys {
			t.Error("account_determination_key_check: got it unvalidated or without the two keys, want it validated with contract_asset and contract_liability")
		}
	})
	t.Run("the revert", func(t *testing.T) {
		down := ""
		for _, m := range Migrations() {
			if strings.HasSuffix(m.Name, "_revenue_recognition") && !strings.Contains(m.Name, "validate") {
				down = m.Down
			}
		}
		if down == "" {
			t.Fatal("got no migration named …_revenue_recognition in db/migrations, want the one the test is about")
		}
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		tenant := id()
		if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
			step(run, tenant, id(), id(), 2, "2026-02-28", 0, 0, 0, 0, 0, 0, 0)); err != nil {
			t.Fatal(err)
		}
		failsWith(t, exec(tx, down), "reverting would lose what later runs take off")
	})
}
