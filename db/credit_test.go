package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCreditPermissions (T77a, migration 000271; ADR-0066 §6): the
// permissions to read and to set credit limits are seeded and are no
// approval permissions.
func TestCreditPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'credit.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "credit.limit.read credit.limit.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestCreditLimitRules (T77a, migration 000271; ADR-0066 §2): a credit
// limit is an amount of zero or more in a known currency, one per
// customer and company; a change sets its amount with its version and
// who changed it, and nothing else of it; no limit is deleted.
func TestCreditLimitRules(t *testing.T) {
	need(t)
	const limit = `INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $1, $1)`
	type scene struct{ tenant, limit, company, customer uuid.UUID }
	set := func(s scene) stmt { return step(limit, s.tenant, s.limit, s.company, s.customer, "BP1", "EUR", 50000) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a limit", func(s scene) []stmt { return []stmt{set(s)} }, ""},
		{"a limit of nothing", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, s.company, s.customer, "BP1", "EUR", 0)}
		}, ""},
		{"a negative limit", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, s.company, s.customer, "BP1", "EUR", -1)}
		}, "credit_limit_amount_check"},
		{"a limit in an unknown currency", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, s.company, s.customer, "BP1", "XXA", 50000)}
		}, "credit_limit_currency_fkey"},
		{"a limit without the customer's number", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, s.company, s.customer, "", "EUR", 50000)}
		}, "credit_limit_customer_number_check"},
		{"the customer's second limit in the company", func(s scene) []stmt {
			return []stmt{set(s), step(limit, s.tenant, id(), s.company, s.customer, "BP1", "EUR", 90000)}
		}, "credit_limit_once"},
		{"the customer's limit in another company", func(s scene) []stmt {
			return []stmt{set(s), step(limit, s.tenant, id(), id(), s.customer, "BP1", "EUR", 90000)}
		}, ""},
		{"another customer's limit in the company", func(s scene) []stmt {
			return []stmt{set(s), step(limit, s.tenant, id(), s.company, id(), "BP2", "EUR", 90000)}
		}, ""},
		{"the amount changed", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET amount = 75000, version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.limit)}
		}, ""},
		{"the amount changed to a negative one", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET amount = -1, version = version + 1 WHERE id = $1`, s.limit)}
		}, "credit_limit_amount_check"},
		{"a limit set at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount, version,
				created_by, updated_by) VALUES ($1, $2, $3, $4, 'BP1', 'EUR', 50000, 0, $1, $1)`, s.tenant, s.limit, s.company, s.customer)}
		}, "credit_limit_version_check"},
		{"the amount changed without the next version", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET amount = 75000 WHERE id = $1`, s.limit)}
		}, "optimistic lock"},
		{"the currency changed", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET currency = 'USD' WHERE id = $1`, s.limit)}
		}, "permission denied"},
		{"the customer changed", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET customer_id = gen_random_uuid() WHERE id = $1`, s.limit)}
		}, "permission denied"},
		{"the customer's number changed", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET customer_number = 'BP2' WHERE id = $1`, s.limit)}
		}, "permission denied"},
		{"the company changed", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET legal_entity_id = gen_random_uuid() WHERE id = $1`, s.limit)}
		}, "permission denied"},
		{"a limit deleted", func(s scene) []stmt {
			return []stmt{set(s), step(`DELETE FROM credit.credit_limit`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), limit: id(), company: id(), customer: id()}
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

// TestCreditLimitTouched (T77a, migration 000271): a change of a limit
// moves its updated_at, which the application cannot set itself.
func TestCreditLimitTouched(t *testing.T) {
	need(t)
	tenant, limit := id(), id()
	var moved bool
	err := asApp(t, tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount,
				created_at, updated_at, created_by, updated_by)
			VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 'EUR', 50000, '2026-01-01', '2026-01-01', $1, $1)`, tenant, limit); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE credit.credit_limit SET amount = 75000, version = version + 1, updated_by = $1 WHERE id = $2
			RETURNING updated_at > '2026-01-01' AND created_at = '2026-01-01' AND version = 2`, tenant, limit).Scan(&moved)
	})
	if err != nil || !moved {
		t.Errorf("got %v and moved %v, want the change to move updated_at alone, at version 2", err, moved)
	}
}
