package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// chargeScene is a confirmed run with one notice, which a dunning charge
// names, and a second notice of another run.
type chargeScene struct{ tenant, run, notice, run2, notice2 uuid.UUID }

const (
	chargeRun = `INSERT INTO collections.dunning_run (tenant_id, id, legal_entity_id, key_date, status, currency, notice_count, item_count,
		overdue_amount, created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), '2026-06-30', 'proposed', 'EUR', 1, 1, 10000, $1, $1)`
	chargeNotice = `INSERT INTO collections.dunning_notice (tenant_id, id, dunning_run_id, customer_id, customer_number, level, item_count, overdue_amount,
		charge_amount) VALUES ($1, $2, $3, gen_random_uuid(), 'BP1', 2, 1, 10000, 1500)`
	// dunningCharge names the notice's memo: $4 its id, $5 its number.
	dunningCharge = `INSERT INTO collections.dunning_charge (tenant_id, dunning_notice_id, dunning_run_id, customer_invoice_id, customer_invoice_number,
		journal_entry_year, journal_entry_id, posting_date) VALUES ($1, $2, $3, $4, $5, 2026, gen_random_uuid(), '2026-07-01')`
)

// charged writes the scene's runs and notices.
func (s chargeScene) charged(tx pgx.Tx) error {
	return all(tx, step(chargeRun, s.tenant, s.run), step(chargeNotice, s.tenant, s.notice, s.run),
		step(chargeRun, s.tenant, s.run2), step(chargeNotice, s.tenant, s.notice2, s.run2))
}

// TestDunningChargeRules (T76h, migration 000395; ADR-0065 As built): a
// dunning charge names a notice of its run and its debit memo by id and a
// number of 1 to 40 characters; a notice is charged once and a memo
// charges one notice; the application changes and deletes none.
func TestDunningChargeRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		then func(s chargeScene) []stmt
		want string // empty: allowed
	}{
		{"a notice charged", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100001")}
		}, ""},
		{"a notice charged twice", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100001"), step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100002")}
		}, "dunning_charge_pkey"},
		{"one memo charging two notices", func(s chargeScene) []stmt {
			memo := id()
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, memo, "CI100001"), step(dunningCharge, s.tenant, s.notice2, s.run2, memo, "CI100001")}
		}, "dunning_charge_invoice_once"},
		{"a notice of another run", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice2, s.run, id(), "CI100001")}
		}, "dunning_charge_notice_fkey"},
		{"a notice that does not exist", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, id(), s.run, id(), "CI100001")}
		}, "dunning_charge_notice_fkey"},
		{"a memo without a number", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), "")}
		}, "dunning_charge_invoice_number_check"},
		{"a memo number of 41 characters", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), strings.Repeat("9", 41))}
		}, "dunning_charge_invoice_number_check"},
		{"a memo number of 40 characters", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), strings.Repeat("9", 40))}
		}, ""},
		{"a charge changed", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100001"),
				step(`UPDATE collections.dunning_charge SET posting_date = '2026-07-02'`)}
		}, "permission denied"},
		{"a charge deleted", func(s chargeScene) []stmt {
			return []stmt{step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100001"), step(`DELETE FROM collections.dunning_charge`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := chargeScene{tenant: id(), run: id(), notice: id(), run2: id(), notice2: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := s.charged(tx); err != nil {
					t.Fatalf("the notices: got %v, want no error", err)
				}
				return all(tx, c.then(s)...)
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
}

// TestDunningChargeFixedForTheOwner (T76h, migration 000395): a charge is
// insert-only for the table's owner too, so no later migration or repair
// rewrites which memo charged a notice.
func TestDunningChargeFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := chargeScene{tenant: id(), run: id(), notice: id(), run2: id(), notice2: id()}
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())); err != nil {
		t.Fatal(err)
	}
	if err := s.charged(tx); err != nil {
		t.Fatal(err)
	}
	if err := all(tx, step(dunningCharge, s.tenant, s.notice, s.run, id(), "CI100001")); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"UPDATE collections.dunning_charge SET posting_date = '2026-07-02' WHERE tenant_id = $1",
		"DELETE FROM collections.dunning_charge WHERE tenant_id = $1"} {
		t.Run(change, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(ctx, change, s.tenant)
			failsWith(t, err, "insert-only")
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
