package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRevenueFollowRules (T81e, migration 000363; ADR-0068, As built
// T81e): a contract follows its sales order only with a kept change
// numbered as its next, its status kept and, active, its shares still the
// transaction price; a follow changes an obligation's order value,
// allocated amount and end, never what a person judged; a person changes
// none of them; a contract ends with its price at zero and every
// obligation ended with nothing allocated, and an ended contract or
// obligation changes no more; the rules hold for the tables' owner too; a
// follow is written once, numbered once within its contract, and never
// changed.
func TestRevenueFollowRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO revenue.contract (tenant_id, id, legal_entity_id, customer_id, sales_order_id, sales_order_number, currency,
		transaction_price, status, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'SO1', 'EUR', 270000, 'draft', $1, $1)`
	const obligation = `INSERT INTO revenue.performance_obligation (tenant_id, contract_id, obligation_no, order_item_no, description, net_amount,
		timing, standalone_selling_price) VALUES ($1, $2, $3, $3, 'Bracket', $4, 'point_in_time', $4)`
	const share = `UPDATE revenue.performance_obligation SET allocated_amount = $2 WHERE obligation_no = $1`
	const activate = `UPDATE revenue.contract SET status = 'active', version = version + 1`
	const kept = `INSERT INTO revenue.contract_change (tenant_id, id, contract_id, change_no, cause, sales_order_version, sales_order_changes,
		transaction_price_before, transaction_price_after, obligations, created_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, 3, 1, 270000, $5, '[]', $1)`
	const net = `UPDATE revenue.performance_obligation SET net_amount = $2 WHERE obligation_no = $1`
	const end = `UPDATE revenue.performance_obligation SET allocated_amount = CASE WHEN allocated_amount IS NULL THEN NULL ELSE 0 END, ended_at = now()`
	const followed = `UPDATE revenue.contract SET transaction_price = $2, changes = $1, version = version + 1`
	const ended = `UPDATE revenue.contract SET transaction_price = 0, changes = $1, ended_at = now(), version = version + 1`
	type scene struct{ tenant, doc uuid.UUID }
	draft := func(s scene, more ...stmt) []stmt {
		return append([]stmt{step(header, s.tenant, s.doc), step(obligation, s.tenant, s.doc, 1, 150000), step(obligation, s.tenant, s.doc, 2, 120000)},
			more...)
	}
	active := func(s scene, more ...stmt) []stmt {
		return draft(s, append([]stmt{step(share, 1, 150000), step(share, 2, 120000), step(activate)}, more...)...)
	}
	change := func(s scene, no int, cause string, after int64) stmt {
		return step(kept, s.tenant, s.doc, no, cause, after)
	}
	// raised is a follow of the order's first item from 1500.00 to 1560.00, the shares as the prices weigh them.
	raised := func(s scene) []stmt {
		return []stmt{change(s, 1, "sales_order_changed", 276000), step(net, 1, 156000), step(share, 1, 153333), step(share, 2, 122667),
			step(followed, 1, 276000)}
	}
	cancelled := func(s scene) []stmt {
		return []stmt{change(s, 1, "sales_order_cancelled", 0), step(end), step(ended, 1)}
	}
	owner := step(`SET LOCAL ROLE erp_owner`)
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a draft followed with its change kept", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 276000), step(net, 1, 156000), step(followed, 1, 276000))
		}, ""},
		{"an active contract followed and allocated again", func(s scene) []stmt { return active(s, raised(s)...) }, ""},
		{"followed twice", func(s scene) []stmt {
			return active(s, append(raised(s), change(s, 2, "sales_order_changed", 270000), step(net, 1, 150000), step(share, 1, 150000),
				step(share, 2, 120000), step(followed, 2, 270000))...)
		}, ""},
		{"an active contract ended", func(s scene) []stmt { return active(s, cancelled(s)...) }, ""},
		{"a draft ended", func(s scene) []stmt { return draft(s, cancelled(s)...) }, ""},
		{"an active contract followed with shares that are not the price", func(s scene) []stmt {
			return active(s, change(s, 1, "sales_order_changed", 276000), step(net, 1, 156000), step(followed, 1, 276000))
		}, "allocated amounts are not its transaction price"},
		{"followed without a change kept", func(s scene) []stmt { return draft(s, step(followed, 1, 276000)) }, "to a kept change"},
		{"followed past the next change", func(s scene) []stmt {
			return draft(s, change(s, 2, "sales_order_changed", 276000), step(followed, 2, 276000))
		}, "to a kept change"},
		{"followed into another status", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 270000), step(`UPDATE revenue.contract SET status = 'active', changes = 1`))
		}, "to a kept change"},
		{"a person's change of the transaction price", func(s scene) []stmt {
			return draft(s, step(`UPDATE revenue.contract SET transaction_price = 1`))
		}, "with a kept follow"},
		{"a person's ending", func(s scene) []stmt { return draft(s, step(`UPDATE revenue.contract SET ended_at = now()`)) }, "with a kept follow"},
		{"a person's change of an order value", func(s scene) []stmt { return draft(s, step(net, 1, 1)) }, "with a kept follow"},
		{"a person's ending of an obligation", func(s scene) []stmt { return draft(s, step(end)) }, "with a kept follow"},
		{"an active contract's order value changed without a follow", func(s scene) []stmt { return active(s, step(net, 1, 1)) },
			"an active contract's obligations are fixed"},
		{"a follow's change of a stand-alone selling price", func(s scene) []stmt {
			return active(s, change(s, 1, "sales_order_changed", 270000),
				step(`UPDATE revenue.performance_obligation SET standalone_selling_price = 1 WHERE obligation_no = 1`))
		}, "a follow keeps what a person judged"},
		{"a follow's change of a timing", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 270000),
				step(`UPDATE revenue.performance_obligation SET timing = 'over_time', service_from = '2026-01-01', service_to = '2026-12-31'`))
		}, "a follow keeps what a person judged"},
		{"a follow's change of the day named", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 270000), step(`UPDATE revenue.performance_obligation SET satisfied_on = '2026-03-01'`))
		}, "a follow keeps what a person judged"},
		{"an obligation added by a follow", func(s scene) []stmt {
			return active(s, change(s, 1, "sales_order_changed", 280000), step(obligation, s.tenant, s.doc, 3, 10000), step(share, 3, 10000),
				step(followed, 1, 280000))
		}, ""},
		{"a price of nothing without the end", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 0), step(followed, 1, 0))
		}, "contract_transaction_price_positive"},
		{"an ended obligation with a share", func(s scene) []stmt {
			return active(s, change(s, 1, "sales_order_cancelled", 0), step(`UPDATE revenue.performance_obligation SET ended_at = now()`))
		}, "performance_obligation_ended_unallocated"},
		{"an ended contract changed", func(s scene) []stmt {
			return active(s, append(cancelled(s), step(`UPDATE revenue.contract SET version = version + 1`))...)
		}, "changes no more"},
		{"an ended contract followed again", func(s scene) []stmt {
			return active(s, append(cancelled(s), change(s, 2, "sales_order_changed", 0), step(ended, 2))...)
		}, "changes no more"},
		{"an ended obligation changed", func(s scene) []stmt {
			return active(s, append(cancelled(s), step(net, 1, 1))...)
		}, "changes no more"},
		{"an ended obligation changed by a later follow", func(s scene) []stmt {
			return active(s, change(s, 1, "sales_order_changed", 120000), step(end+` WHERE obligation_no = 1`), step(share, 2, 120000),
				step(followed, 1, 120000), change(s, 2, "sales_order_changed", 120000), step(net, 1, 1))
		}, "changes no more"},
		{"the rule for the tables' owner", func(s scene) []stmt { return draft(s, owner, step(followed, 1, 276000)) }, "to a kept change"},
		{"the obligations' rule for the tables' owner", func(s scene) []stmt {
			return active(s, owner, change(s, 1, "sales_order_changed", 270000),
				step(`UPDATE revenue.performance_obligation SET standalone_selling_price = 1 WHERE obligation_no = 1`))
		}, "a follow keeps what a person judged"},
		{"a follow numbered twice", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 276000), change(s, 1, "sales_order_changed", 276000))
		}, "contract_change_once"},
		{"a follow of an unknown cause", func(s scene) []stmt { return draft(s, change(s, 1, "sales_order_lost", 276000)) }, "contract_change_cause_check"},
		{"a follow changed", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 276000), owner, step(`UPDATE revenue.contract_change SET cause = 'sales_order_cancelled'`))
		}, "is insert-only"},
		{"a follow deleted", func(s scene) []stmt {
			return draft(s, change(s, 1, "sales_order_changed", 276000), owner, step(`DELETE FROM revenue.contract_change`))
		}, "is insert-only"},
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

// TestRevenueFollowRevert (T81e, migration 000363's down): the revert is
// refused, before it changes anything, while a contract has followed its
// order.
func TestRevenueFollowRevert(t *testing.T) {
	need(t)
	down := ""
	for _, m := range Migrations() {
		if strings.HasSuffix(m.Name, "_revenue_contract_follows_order") {
			down = m.Down
		}
	}
	if down == "" {
		t.Fatal("got no migration named …_revenue_contract_follows_order in db/migrations, want the one the test is about")
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
		step(`INSERT INTO revenue.contract_change (tenant_id, id, contract_id, change_no, cause, sales_order_version, sales_order_changes,
			transaction_price_before, transaction_price_after, obligations, created_by)
			VALUES ($1, gen_random_uuid(), $2, 1, 'sales_order_changed', 3, 1, 100, 120, '[]', $1)`, tenant, doc)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would drop them")
}
