package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLeasePermissions (T88a and T88b, migrations 000326 and 000327;
// ADR-0073 §6): the permissions of lease contracts are seeded and are no
// approval permissions.
func TestLeasePermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'leases.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "leases.contract.activate leases.contract.read leases.contract.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestLeaseContractRules (T88a, migration 000326; ADR-0073 §3-§4): a
// contract has a number once per company, a term of 1 to 600 months that
// its frequency divides, a known frequency and timing, a payment above
// zero, a discount rate above zero and at most 100, and is a draft; it is
// neither changed nor deleted. Its payments are numbered 1 to 600, one a
// day, above zero, of a contract that exists, and are insert-only.
func TestLeaseContractRules(t *testing.T) {
	need(t)
	const contract = `INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $1, $1, '2026-01-01', $4, $5, 'EUR', $6, $7, $8, $1, $9, $1, $1)`
	const payment = `INSERT INTO leases.payment (tenant_id, contract_id, payment_no, due_date, amount) VALUES ($1, $2, $3, $4, $5)`
	type scene struct{ tenant, contract uuid.UUID }
	with := func(s scene, number string, term, amount int, frequency, timing, rate, status string) stmt {
		return step(contract, s.tenant, s.contract, number, term, amount, frequency, timing, rate, status)
	}
	create := func(s scene) stmt { return with(s, "LC000001", 12, 100, "monthly", "advance", "5", "draft") }
	pay := func(s scene, no int, due string, amount int) stmt {
		return step(payment, s.tenant, s.contract, no, due, amount)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a contract", func(s scene) []stmt { return []stmt{create(s)} }, ""},
		{"an empty number", func(s scene) []stmt { return []stmt{with(s, "", 12, 100, "monthly", "advance", "5", "draft")} }, "contract_document_number_check"},
		{"a number of 41 characters", func(s scene) []stmt {
			return []stmt{with(s, strings.Repeat("L", 41), 12, 100, "monthly", "advance", "5", "draft")}
		}, "contract_document_number_check"},
		{"a number taken in the company", func(s scene) []stmt {
			return []stmt{create(s), step(contract, s.tenant, id(), "LC000001", 12, 100, "monthly", "advance", "5", "draft")}
		}, "contract_number_once"},
		{"a term of no months", func(s scene) []stmt { return []stmt{with(s, "LC1", 0, 100, "monthly", "advance", "5", "draft")} }, "contract_term_months_check"},
		{"a term of 601 months", func(s scene) []stmt { return []stmt{with(s, "LC1", 601, 100, "monthly", "advance", "5", "draft")} }, "contract_term_months_check"},
		{"a term of 600 months", func(s scene) []stmt { return []stmt{with(s, "LC1", 600, 100, "yearly", "advance", "5", "draft")} }, ""},
		{"a payment of zero", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 0, "monthly", "advance", "5", "draft")} }, "contract_payment_amount_positive"},
		{"an unknown frequency", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "weekly", "advance", "5", "draft")} }, "contract_payment_frequency_check"},
		{"an unknown timing", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "midway", "5", "draft")} }, "contract_payment_timing_check"},
		{"a quarterly frequency of a term of 10 months", func(s scene) []stmt {
			return []stmt{with(s, "LC1", 10, 100, "quarterly", "advance", "5", "draft")}
		}, "contract_frequency_divides_term"},
		{"a yearly frequency of a term of 18 months", func(s scene) []stmt {
			return []stmt{with(s, "LC1", 18, 100, "yearly", "advance", "5", "draft")}
		}, "contract_frequency_divides_term"},
		{"a rate of zero", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "advance", "0", "draft")} }, "contract_annual_rate_check"},
		{"a rate above 100", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "advance", "100.5", "draft")} }, "contract_annual_rate_check"},
		{"a rate of 100", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "advance", "100", "draft")} }, ""},
		{"an unknown status", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "advance", "5", "closed")} }, "contract_status_check"},
		{"created active", func(s scene) []stmt { return []stmt{with(s, "LC1", 12, 100, "monthly", "advance", "5", "active")} }, "contract_activation_check"},
		{"created at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
					payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, version, created_by, updated_by)
				VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 12, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', 0, $1, $1)`, s.tenant, s.contract)}
		}, "contract_version_check"},
		{"its terms changed", func(s scene) []stmt {
			return []stmt{create(s), step(`UPDATE leases.contract SET term_months = 24, version = version + 1 WHERE id = $1`, s.contract)}
		}, "permission denied"},
		{"a contract deleted", func(s scene) []stmt { return []stmt{create(s), step(`DELETE FROM leases.contract`)} }, "permission denied"},
		{"its payments", func(s scene) []stmt {
			return []stmt{create(s), pay(s, 1, "2026-01-01", 100), pay(s, 2, "2026-02-01", 100)}
		}, ""},
		{"a payment numbered 0", func(s scene) []stmt { return []stmt{create(s), pay(s, 0, "2026-01-01", 100)} }, "payment_no_check"},
		{"a payment numbered 601", func(s scene) []stmt { return []stmt{create(s), pay(s, 601, "2026-01-01", 100)} }, "payment_no_check"},
		{"a payment of zero", func(s scene) []stmt { return []stmt{create(s), pay(s, 1, "2026-01-01", 0)} }, "payment_amount_positive"},
		{"a payment numbered twice", func(s scene) []stmt {
			return []stmt{create(s), pay(s, 1, "2026-01-01", 100), pay(s, 1, "2026-02-01", 100)}
		}, "payment_pkey"},
		{"two payments on a day", func(s scene) []stmt {
			return []stmt{create(s), pay(s, 1, "2026-01-01", 100), pay(s, 2, "2026-01-01", 100)}
		}, "payment_date_once"},
		{"a payment of no contract", func(s scene) []stmt { return []stmt{pay(s, 1, "2026-01-01", 100)} }, "payment_contract_fkey"},
		{"a payment changed", func(s scene) []stmt {
			return []stmt{create(s), pay(s, 1, "2026-01-01", 100), step(`UPDATE leases.payment SET amount = 200`)}
		}, "permission denied"},
		{"a payment deleted", func(s scene) []stmt {
			return []stmt{create(s), pay(s, 1, "2026-01-01", 100), step(`DELETE FROM leases.payment`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), contract: id()}
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

// activate is the update that activates the scene's contract with the
// liability, the right-of-use amount and an entry.
func activate(contract uuid.UUID, liability, rightOfUse int) stmt {
	return step(`UPDATE leases.contract SET status = 'active', version = version + 1, lease_liability = $2, right_of_use_amount = $3,
		journal_entry_id = gen_random_uuid(), journal_entry_year = 2026, journal_entry_number = '100001' WHERE id = $1`, contract, liability, rightOfUse)
}

// TestLeaseActivationRules (T88b, migration 000327; ADR-0073 §7-§8): an
// active contract names its liability, its right-of-use amount, which is
// not below the liability, and its entry, and a draft none; an active
// contract is fixed. A schedule line's liability moves by its interest and
// payment alone, its amounts but the interest are not negative, its
// period runs forward, it belongs to a contract and is insert-only.
func TestLeaseActivationRules(t *testing.T) {
	need(t)
	const contract = `INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 12, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', $1, $1)`
	const line = `INSERT INTO leases.schedule_line (tenant_id, contract_id, line_no, period_start, period_end, opening_liability, payment, interest,
			closing_liability, depreciation, right_of_use_closing)
		VALUES ($1, $2, $3, $4, '2026-01-31', $5, $6, $7, $8, $9, $10)`
	type scene struct{ tenant, contract uuid.UUID }
	create := func(s scene) stmt { return step(contract, s.tenant, s.contract) }
	lineOf := func(s scene, no int, start string, opening, payment, interest, closing, depreciation, carrying int) stmt {
		return step(line, s.tenant, s.contract, no, start, opening, payment, interest, closing, depreciation, carrying)
	}
	ok := func(s scene) stmt { return lineOf(s, 1, "2026-01-01", 1000, 100, 5, 905, 90, 910) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"activated", func(s scene) []stmt { return []stmt{create(s), activate(s.contract, 900, 1000)} }, ""},
		{"activated without an entry", func(s scene) []stmt {
			return []stmt{create(s), step(`UPDATE leases.contract SET status = 'active', version = version + 1, lease_liability = 900,
				right_of_use_amount = 1000 WHERE id = $1`, s.contract)}
		}, "contract_activation_check"},
		{"a draft with an entry", func(s scene) []stmt {
			return []stmt{create(s), step(`UPDATE leases.contract SET version = version + 1, journal_entry_id = gen_random_uuid() WHERE id = $1`, s.contract)}
		}, "contract_activation_check"},
		{"a right-of-use amount below the liability", func(s scene) []stmt { return []stmt{create(s), activate(s.contract, 900, 899)} },
			"contract_right_of_use_covers_liability"},
		{"a right-of-use amount of zero", func(s scene) []stmt { return []stmt{create(s), activate(s.contract, 0, 0)} }, "contract_right_of_use_amount_check"},
		{"an entry number of 41 characters", func(s scene) []stmt {
			return []stmt{create(s), step(`UPDATE leases.contract SET status = 'active', version = version + 1, lease_liability = 900,
				right_of_use_amount = 1000, journal_entry_id = gen_random_uuid(), journal_entry_year = 2026, journal_entry_number = $2 WHERE id = $1`,
				s.contract, strings.Repeat("9", 41))}
		}, "contract_journal_entry_number_check"},
		{"a negative liability", func(s scene) []stmt { return []stmt{create(s), activate(s.contract, -1, 10)} }, "contract_lease_liability_check"},
		{"an active contract changed", func(s scene) []stmt {
			return []stmt{create(s), activate(s.contract, 900, 1000),
				step(`UPDATE leases.contract SET status = 'draft', version = version + 1 WHERE id = $1`, s.contract)}
		}, "lease_contract_fixed_once_active"},
		{"a line", func(s scene) []stmt { return []stmt{create(s), ok(s)} }, ""},
		{"a line whose liability moves by more", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 1, "2026-01-01", 1000, 100, 5, 906, 90, 910)}
		}, "schedule_line_balance_check"},
		{"a negative interest", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 1, "2026-01-01", 1000, 100, -1, 899, 90, 910)}
		}, ""},
		{"a negative opening", func(s scene) []stmt { return []stmt{create(s), lineOf(s, 1, "2026-01-01", -1, 0, 1, 0, 0, 0)} }, "schedule_line_opening_check"},
		{"a negative payment", func(s scene) []stmt { return []stmt{create(s), lineOf(s, 1, "2026-01-01", 0, -1, 0, 1, 0, 0)} }, "schedule_line_payment_check"},
		{"a negative closing", func(s scene) []stmt { return []stmt{create(s), lineOf(s, 1, "2026-01-01", 0, 1, 0, -1, 0, 0)} }, "schedule_line_closing_check"},
		{"a negative depreciation", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 1, "2026-01-01", 1000, 100, 5, 905, -1, 910)}
		}, "schedule_line_depreciation_check"},
		{"a negative carrying amount", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 1, "2026-01-01", 1000, 100, 5, 905, 90, -1)}
		}, "schedule_line_right_of_use_check"},
		{"a line numbered 0", func(s scene) []stmt { return []stmt{create(s), lineOf(s, 0, "2026-01-01", 1000, 100, 5, 905, 90, 910)} }, "schedule_line_no_check"},
		{"a line numbered 601", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 601, "2026-01-01", 1000, 100, 5, 905, 90, 910)}
		}, "schedule_line_no_check"},
		{"a period ending before it starts", func(s scene) []stmt {
			return []stmt{create(s), lineOf(s, 1, "2026-02-01", 1000, 100, 5, 905, 90, 910)}
		}, "schedule_line_period_check"},
		{"a line twice", func(s scene) []stmt { return []stmt{create(s), ok(s), ok(s)} }, "schedule_line_pkey"},
		{"a line of no contract", func(s scene) []stmt { return []stmt{ok(s)} }, "schedule_line_contract_fkey"},
		{"a line changed", func(s scene) []stmt {
			return []stmt{create(s), ok(s), step(`UPDATE leases.schedule_line SET interest = 6`)}
		}, "permission denied"},
		{"a line deleted", func(s scene) []stmt { return []stmt{create(s), ok(s), step(`DELETE FROM leases.schedule_line`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), contract: id()}
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

// TestLeaseContractFixedForTheOwner (T88b, migration 000327): an active
// contract stays as it was activated for the owner of the table too, whom
// no grant holds back.
func TestLeaseContractFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, contract := id(), id()
	if err := all(tx, step(`INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 12, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', $1, $1)`, tenant, contract),
		activate(contract, 900, 1000)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, `UPDATE leases.contract SET term_months = 24, version = version + 1 WHERE id = $1`, contract), "lease_contract_fixed_once_active")
}

// TestLeaseAccountsAndJournal (T88b, migrations 000327 and 000328;
// ADR-0073 §7): the account determination takes the right-of-use asset and
// lease liability keys, and the journal takes entries of the leases
// context; both checks are validated, the journal's on every partition.
func TestLeaseAccountsAndJournal(t *testing.T) {
	need(t)
	f := newOrders(t)
	for _, key := range []string{"right_of_use_asset", "lease_liability"} {
		t.Run(key, func(t *testing.T) {
			mustApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,$3,$4,$5)",
					f.tenant, f.entity, key, f.expense, f.actor)
			})
		})
	}
	t.Run("an entry of the leases context", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error { return f.entryFrom(tx, "leases", nil, nil) })
	})
	t.Run("validated", func(t *testing.T) {
		var tables, checked int
		if err := testDB.QueryRow(t.Context(), `SELECT (SELECT count(*) + 2 FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass),
				(SELECT count(*) FROM pg_constraint WHERE conname IN ('journal_entry_source_context_check', 'account_determination_key_check')
					AND convalidated)`).Scan(&tables, &checked); err != nil {
			t.Fatal(err)
		}
		if checked != tables {
			t.Errorf("validated checks: got %d, want %d (the journal, its partitions and the account determination)", checked, tables)
		}
	})
}

// TestLeaseActivationRevert (T88b, migration 000327's down): the revert is
// refused, before it changes anything, while an active contract exists,
// whose activation it would drop.
func TestLeaseActivationRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000327_lease_activation").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, contract := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()),
		step(`INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
			VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 12, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', $1, $1)`, tenant, contract),
		activate(contract, 900, 1000)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would drop what their activation posted")
}
