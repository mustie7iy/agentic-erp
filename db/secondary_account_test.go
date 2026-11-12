package db

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestSecondaryCostElements (T60g, ADR-0064 §4, migration 000277): an
// account's cost element may be secondary on an expense account alone; a
// line on such an account posts only in an entry of the controlling
// context, every other context's and a migration run's refused, naming
// the constraint the API maps to ledger.secondary_account; and an account
// is a secondary cost element from its creation and stays one
// (gl_account_secondary_kept, mapped to ledger.secondary_account_not_new).
func TestSecondaryCostElements(t *testing.T) {
	need(t)
	f := newFixture(t)
	account := func(tx pgx.Tx, number, kind, element string) (uuid.UUID, error) {
		a := id()
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, cost_element, status, created_by, updated_by)
			VALUES ($1,$2,$3,$4,'Account',$5,$6,'active',$7,$7)`, f.tenant, a, f.chart, number, kind, element, f.actor); err != nil {
			return a, err
		}
		return a, exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
			f.tenant, f.entity, a, f.actor)
	}
	var secondary, primary uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if secondary, err = account(tx, "695000", "expense", "secondary"); err != nil {
			return err
		}
		primary, err = account(tx, "696000", "expense", "primary_cost")
		return err
	})
	for _, kind := range []string{"asset", "liability", "equity", "revenue"} {
		t.Run("a secondary cost element on "+kind, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
				_, err := account(tx, "697000", kind, "secondary")
				return err
			}), "gl_account_cost_element")
		})
	}
	for _, tc := range []struct {
		source string
		ok     bool
	}{{"controlling", true}, {"ledger", false}, {"payables", false}, {"payments", false}, {"procurement", false}, {"receivables", false},
		{"assets", false}, {"integration", false}} {
		t.Run("a line from "+tc.source, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return f.sourced(tx, tc.source, line{account: secondary, amount: 1000}, line{account: secondary, amount: -1000})
			})
			if tc.ok {
				if err != nil {
					t.Fatalf("a line on the secondary cost element from %s: got %v, want it posted", tc.source, err)
				}
				return
			}
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.ConstraintName != "journal_line_secondary_account" {
				t.Fatalf("a line on the secondary cost element from %s: got %v, want journal_line_secondary_account", tc.source, err)
			}
		})
	}
	t.Run("controlling's entries post to other accounts too", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return f.sourced(tx, "controlling", line{account: primary, amount: 1000}, line{account: primary, amount: -1000})
		})
	})
	const turn = `UPDATE ledger.gl_account SET cost_element = $2, version = version + 1 WHERE id = $1`
	for name, c := range map[string]struct {
		account uuid.UUID
		element string
	}{
		"a secondary cost element turned into a primary cost account": {secondary, "primary_cost"},
		"a secondary cost element turned into none":                   {secondary, "none"},
		"a primary cost account turned into a secondary cost element": {primary, "secondary"},
	} {
		t.Run(name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, turn, c.account, c.element) })
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.ConstraintName != "gl_account_secondary_kept" {
				t.Fatalf("got %v, want gl_account_secondary_kept", err)
			}
		})
	}
	t.Run("what stays allowed", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			// The activation writes every account's cost element again, and a
			// primary cost account may stop being controlled.
			if err := exec(tx, `UPDATE ledger.gl_account SET name = 'Assessed overhead', cost_element = 'secondary', version = version + 1 WHERE id = $1`, secondary); err != nil {
				return err
			}
			return exec(tx, turn, primary, "none")
		})
	})
}
