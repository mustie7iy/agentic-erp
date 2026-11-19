package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestCreditPermissions (T77a, migration 000279; ADR-0066 §6): the
// permissions to read and to set credit limits are seeded and are no
// approval permissions; approving their changes is one (T77d, 000393).
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
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'credit.limit.approve'`).Scan(&approval); err != nil || !approval {
		t.Errorf("credit.limit.approve: got %v, %v, want an approval permission", approval, err)
	}
}

// TestCreditLimitRules (T77a, migration 000279; ADR-0066 §2): a credit
// limit is an amount of zero or more in a known currency, one per
// customer and company; a change sets its amount, its validity (ending on
// its first day or later) and its review date with its version and who
// changed it, and nothing else of it; no limit is deleted (T77c,
// migration 000392).
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
		{"a validity and a review date set (T77c, 000392)", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET valid_from = '2027-01-01', valid_to = '2027-12-31', next_review_on = '2027-10-01',
				version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.limit)}
		}, ""},
		{"a validity of one day", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET valid_from = '2027-01-01', valid_to = '2027-01-01', version = version + 1 WHERE id = $1`, s.limit)}
		}, ""},
		{"a validity ending before it starts", func(s scene) []stmt {
			return []stmt{set(s), step(`UPDATE credit.credit_limit SET valid_from = '2027-01-02', valid_to = '2027-01-01', version = version + 1 WHERE id = $1`, s.limit)}
		}, "credit_limit_validity_check"},
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

// TestCreditLimitTouched (T77a, migration 000279): a change of a limit
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
	if err != nil {
		t.Fatalf("got %v, want the amount changed", err)
	}
	if !moved {
		t.Error("the change: got another updated_at, created_at or version, want updated_at moved alone, at version 2")
	}
}

// TestCreditLimitChangeRules (T77d, migration 000393; ADR-0066 As built
// T77d): a limit is pending or active, and an active one stays active
// (the trigger, as the owner too); a change of a limit is a create
// without values before or a change with its amount before, asks for an
// amount of zero or more and a validity ending on its first day or later,
// is decided once with its time, one pending per limit; it is linked and
// decided and nothing else of it changes, and no change is deleted.
func TestCreditLimitChangeRules(t *testing.T) {
	need(t)
	const limit = `INSERT INTO credit.credit_limit (tenant_id, id, legal_entity_id, customer_id, customer_number, currency, amount, status, created_by, updated_by)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'BP1', 'EUR', 50000, $3, $1, $1)`
	const change = `INSERT INTO credit.credit_limit_change (tenant_id, id, credit_limit_id, kind, amount, amount_before, valid_from, valid_to, status,
		decided_at, requested_by, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $1, $1, $1)`
	type scene struct{ tenant, limit, change uuid.UUID }
	pending := func(s scene) stmt { return step(limit, s.tenant, s.limit, "pending") }
	create := func(s scene) stmt {
		return step(change, s.tenant, s.change, s.limit, "create", 50000, nil, nil, nil, "pending", nil)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a pending limit with its create", func(s scene) []stmt { return []stmt{pending(s), create(s)} }, ""},
		{"a limit of another status", func(s scene) []stmt { return []stmt{step(limit, s.tenant, s.limit, "blocked")} }, "credit_limit_status_check"},
		{"a pending limit made active", func(s scene) []stmt {
			return []stmt{pending(s), step(`UPDATE credit.credit_limit SET status = 'active', version = version + 1 WHERE id = $1`, s.limit)}
		}, ""},
		{"an active limit made pending", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, "active"), step(`UPDATE credit.credit_limit SET status = 'pending', version = version + 1 WHERE id = $1`, s.limit)}
		}, "credit_limit_status_guard"},
		{"a change with its amount before", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, "active"), step(change, s.tenant, s.change, s.limit, "change", 90000, 50000, "2027-01-01", "2027-12-31", "pending", nil)}
		}, ""},
		{"a change without its amount before", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, "active"), step(change, s.tenant, s.change, s.limit, "change", 90000, nil, nil, nil, "pending", nil)}
		}, "credit_limit_change_before_check"},
		{"a create with an amount before", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", 90000, 50000, nil, nil, "pending", nil)}
		}, "credit_limit_change_before_check"},
		{"a create with a validity before", func(s scene) []stmt {
			return []stmt{pending(s), step(`INSERT INTO credit.credit_limit_change (tenant_id, id, credit_limit_id, kind, amount, valid_to_before, status,
				requested_by, created_by, updated_by) VALUES ($1, $2, $3, 'create', 1, '2027-01-01', 'pending', $1, $1, $1)`, s.tenant, s.change, s.limit)}
		}, "credit_limit_change_before_check"},
		{"another kind", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "raise", 50000, 1, nil, nil, "pending", nil)}
		}, "credit_limit_change_kind_check"},
		{"a negative amount", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", -1, nil, nil, nil, "pending", nil)}
		}, "credit_limit_change_amount_check"},
		{"a negative amount before", func(s scene) []stmt {
			return []stmt{step(limit, s.tenant, s.limit, "active"), step(change, s.tenant, s.change, s.limit, "change", 1, -1, nil, nil, "pending", nil)}
		}, "credit_limit_change_amount_before_check"},
		{"a validity ending before it starts", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", 1, nil, "2027-01-02", "2027-01-01", "pending", nil)}
		}, "credit_limit_change_validity_check"},
		{"another status", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", 1, nil, nil, nil, "approved", "2027-01-01")}
		}, "credit_limit_change_status_check"},
		{"a pending change with a decision time", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", 1, nil, nil, nil, "pending", "2027-01-01")}
		}, "credit_limit_change_decided_check"},
		{"an applied change without one", func(s scene) []stmt {
			return []stmt{pending(s), step(change, s.tenant, s.change, s.limit, "create", 1, nil, nil, nil, "applied", nil)}
		}, "credit_limit_change_decided_check"},
		{"a change of a limit that does not exist", func(s scene) []stmt { return []stmt{create(s)} }, "credit_limit_change_limit_fkey"},
		{"a second pending change", func(s scene) []stmt {
			return []stmt{pending(s), create(s), step(change, s.tenant, id(), s.limit, "create", 1, nil, nil, nil, "pending", nil)}
		}, "credit_limit_change_one_pending"},
		{"a second change once the first is decided", func(s scene) []stmt {
			return []stmt{pending(s), create(s),
				step(`UPDATE credit.credit_limit_change SET status = 'rejected', decided_at = now(), version = version + 1 WHERE id = $1`, s.change),
				step(change, s.tenant, id(), s.limit, "create", 1, nil, nil, nil, "pending", nil)}
		}, ""},
		{"a change linked to its request", func(s scene) []stmt {
			return []stmt{pending(s), create(s),
				step(`UPDATE credit.credit_limit_change SET workflow_request_id = gen_random_uuid(), version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.change)}
		}, ""},
		{"a decided change decided again", func(s scene) []stmt {
			return []stmt{pending(s), create(s),
				step(`UPDATE credit.credit_limit_change SET status = 'rejected', decided_at = now(), version = version + 1 WHERE id = $1`, s.change),
				step(`UPDATE credit.credit_limit_change SET status = 'applied', version = version + 1 WHERE id = $1`, s.change)}
		}, "credit_limit_change_decided_once"},
		{"the amount asked for changed", func(s scene) []stmt {
			return []stmt{pending(s), create(s), step(`UPDATE credit.credit_limit_change SET amount = 1, version = version + 1 WHERE id = $1`, s.change)}
		}, "permission denied"},
		{"a change deleted", func(s scene) []stmt {
			return []stmt{pending(s), create(s), step(`DELETE FROM credit.credit_limit_change`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), limit: id(), change: id()}
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
	t.Run("as the owner", func(t *testing.T) {
		s := scene{tenant: id(), limit: id(), change: id()}
		failsWith(t, asApp(t, s.tenant, asOwner(func(tx pgx.Tx) error {
			return all(tx, step(limit, s.tenant, s.limit, "active"), step(`UPDATE credit.credit_limit SET status = 'pending', version = version + 1 WHERE id = $1`, s.limit))
		})), "credit_limit_status_guard")
		failsWith(t, asApp(t, s.tenant, asOwner(func(tx pgx.Tx) error {
			return all(tx, pending(s), create(s),
				step(`UPDATE credit.credit_limit_change SET status = 'cancelled', decided_at = now(), version = version + 1 WHERE id = $1`, s.change),
				step(`UPDATE credit.credit_limit_change SET amount = 1, version = version + 1 WHERE id = $1`, s.change))
		})), "credit_limit_change_decided_once")
	})
}
