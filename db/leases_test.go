package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLeasePermissions (T88a, T88b and T88c, migrations 000326, 000327
// and 000331; ADR-0073 §6): the permissions of lease contracts are seeded and are no
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
	if got, want := strings.Join(codes, " "), "leases.contract.activate leases.contract.read leases.contract.write leases.run.post"; got != want {
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

// TestLeaseAccountsAndJournal (T88b and T88c, migrations 000327, 000328,
// 000331 and 000332; ADR-0073 §7, §9): the account determination takes the
// right-of-use asset and lease liability keys and the lease run's three, and the journal takes entries of the leases
// context; both checks are validated, the journal's on every partition.
func TestLeaseAccountsAndJournal(t *testing.T) {
	need(t)
	f := newOrders(t)
	for _, key := range []string{"right_of_use_asset", "lease_liability", "lease_interest", "right_of_use_depreciation", "right_of_use_accumulated_depreciation"} {
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

// leaseRunScene is a contract with three stored schedule lines, ending on
// the last days of January, February and March 2026, in a tenant.
type leaseRunScene struct{ tenant, contract, run uuid.UUID }

func (s leaseRunScene) setup() []stmt {
	out := []stmt{step(`INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 3, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', $1, $1)`, s.tenant, s.contract)}
	for no, end := range []string{"2026-01-31", "2026-02-28", "2026-03-31"} {
		out = append(out, step(`INSERT INTO leases.schedule_line (tenant_id, contract_id, line_no, period_start, period_end, opening_liability, payment,
				interest, closing_liability, depreciation, right_of_use_closing) VALUES ($1, $2, $3, $4::date - 27, $4, 0, 0, 0, 0, 0, 0)`,
			s.tenant, s.contract, no+1, end))
	}
	return out
}

// leaseRun is a run of the scene's company (the tenant's id) for the
// period of 2026 ending on the day, with the counts and amounts.
func leaseRun(s leaseRunScene, run uuid.UUID, period int, day string, contracts, postings, interest, depreciation int) stmt {
	return step(`INSERT INTO leases.run (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, posting_date, currency, contract_count,
			posting_count, interest_amount, depreciation_amount, created_by) VALUES ($1, $2, $1, 2026, $3, $4, 'EUR', $5, $6, $7, $8, $1)`,
		s.tenant, run, period, day, contracts, postings, interest, depreciation)
}

// leaseRunPosting is a posting of the run for the scene's contract, its
// lines first to last.
func leaseRunPosting(s leaseRunScene, run uuid.UUID, first, last, interest, depreciation int, number string) stmt {
	return step(`INSERT INTO leases.run_posting (tenant_id, run_id, contract_id, id, first_line_no, last_line_no, interest_amount, depreciation_amount,
			journal_entry_id, journal_entry_year, journal_entry_number) VALUES ($1, $2, $3, gen_random_uuid(), $4, $5, $6, $7, gen_random_uuid(), 2026, $8)`,
		s.tenant, run, s.contract, first, last, interest, depreciation, number)
}

// TestLeaseRunRules (T88c, migration 000331; ADR-0073 §9): a run is one
// per company and period, each for a later period than the company's
// last, counts no more postings than contracts, moves nothing when it
// posted nothing, and is insert-only. A posting names lines of its
// contract's schedule, in order, each posted once, moves something, has
// no negative depreciation, belongs to a run and is insert-only.
func TestLeaseRunRules(t *testing.T) {
	need(t)
	january := func(s leaseRunScene) stmt { return leaseRun(s, s.run, 1, "2026-01-31", 1, 1, 5, 90) }
	posting := func(s leaseRunScene) stmt { return leaseRunPosting(s, s.run, 1, 1, 5, 90, "100001") }
	with := func(s leaseRunScene, more ...stmt) []stmt { return append(s.setup(), more...) }
	cases := []struct {
		name string
		run  func(s leaseRunScene) []stmt
		want string // empty: allowed
	}{
		{"a run with its posting", func(s leaseRunScene) []stmt { return with(s, january(s), posting(s)) }, ""},
		{"a run posting nothing", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 1, "2026-01-31", 1, 0, 0, 0)) }, ""},
		{"a run posting nothing with interest", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 1, "2026-01-31", 1, 0, 1, 0)) },
			"run_nothing_posted_check"},
		{"more postings than contracts", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 1, "2026-01-31", 1, 2, 5, 90)) },
			"run_posting_count_check"},
		{"a negative contract count", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 1, "2026-01-31", -1, 0, 0, 0)) },
			"run_contract_count_check"},
		{"a negative depreciation", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 1, "2026-01-31", 1, 1, 5, -1)) },
			"run_depreciation_check"},
		{"period 0", func(s leaseRunScene) []stmt { return with(s, leaseRun(s, s.run, 0, "2026-01-31", 1, 1, 5, 90)) }, "run_period_check"},
		{"a second run of the period", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRun(s, id(), 1, "2026-01-31", 1, 0, 0, 0))
		}, "lease_run_once"},
		{"a run for an earlier period", func(s leaseRunScene) []stmt {
			return with(s, leaseRun(s, s.run, 2, "2026-02-28", 1, 0, 0, 0), leaseRun(s, id(), 1, "2026-01-31", 1, 0, 0, 0))
		}, "lease_run_in_order"},
		{"a run for a later period", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRun(s, id(), 2, "2026-02-28", 1, 0, 0, 0))
		}, ""},
		{"a run changed", func(s leaseRunScene) []stmt {
			return with(s, january(s), step(`UPDATE leases.run SET contract_count = 2`))
		}, "permission denied"},
		{"a run deleted", func(s leaseRunScene) []stmt { return with(s, january(s), step(`DELETE FROM leases.run`)) }, "permission denied"},
		{"a posting of lines 1 to 3", func(s leaseRunScene) []stmt { return with(s, january(s), leaseRunPosting(s, s.run, 1, 3, 5, 90, "1")) }, ""},
		{"a posting of no run", func(s leaseRunScene) []stmt { return with(s, posting(s)) }, "run_posting_run_fkey"},
		{"a posting of no contract", func(s leaseRunScene) []stmt { return []stmt{january(s), posting(s)} }, "run_posting_contract_fkey"},
		{"a posting from a line the schedule lacks", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRunPosting(s, s.run, 0, 1, 5, 90, "1"))
		}, "run_posting_first_line_fkey"},
		{"a posting to a line the schedule lacks", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRunPosting(s, s.run, 1, 4, 5, 90, "1"))
		}, "run_posting_last_line_fkey"},
		{"lines backwards", func(s leaseRunScene) []stmt { return with(s, january(s), leaseRunPosting(s, s.run, 2, 1, 5, 90, "1")) }, "run_posting_lines_check"},
		{"a posting moving nothing", func(s leaseRunScene) []stmt { return with(s, january(s), leaseRunPosting(s, s.run, 1, 1, 0, 0, "1")) },
			"run_posting_moves_check"},
		{"a negative interest", func(s leaseRunScene) []stmt { return with(s, january(s), leaseRunPosting(s, s.run, 1, 1, -1, 90, "1")) }, ""},
		{"a posting's negative depreciation", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRunPosting(s, s.run, 1, 1, 5, -1, "1"))
		}, "run_posting_depreciation_check"},
		{"an entry number of 41 characters", func(s leaseRunScene) []stmt {
			return with(s, january(s), leaseRunPosting(s, s.run, 1, 1, 5, 90, strings.Repeat("9", 41)))
		}, "run_posting_journal_entry_number_check"},
		{"a line posted twice", func(s leaseRunScene) []stmt {
			later := id()
			return with(s, january(s), leaseRunPosting(s, s.run, 1, 2, 5, 90, "1"), leaseRun(s, later, 3, "2026-03-31", 1, 1, 5, 90),
				leaseRunPosting(s, later, 2, 3, 5, 90, "2"))
		}, "lease_line_posted_once"},
		{"the next lines posted", func(s leaseRunScene) []stmt {
			later := id()
			return with(s, january(s), posting(s), leaseRun(s, later, 3, "2026-03-31", 1, 1, 5, 90), leaseRunPosting(s, later, 2, 3, 5, 90, "2"))
		}, ""},
		{"a posting changed", func(s leaseRunScene) []stmt {
			return with(s, january(s), posting(s), step(`UPDATE leases.run_posting SET interest_amount = 6`))
		}, "permission denied"},
		{"a posting deleted", func(s leaseRunScene) []stmt {
			return with(s, january(s), posting(s), step(`DELETE FROM leases.run_posting`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := leaseRunScene{tenant: id(), contract: id(), run: id()}
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

// TestLeaseRunRulesForTheOwner (T88c, migration 000331): the runs'
// order and a line posted once hold for the tables' owner too, whom no
// grant holds back.
func TestLeaseRunRulesForTheOwner(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		run  func(s leaseRunScene) []stmt
		want string
	}{
		"a run for an earlier period": {func(s leaseRunScene) []stmt {
			return []stmt{leaseRun(s, s.run, 2, "2026-02-28", 1, 0, 0, 0), leaseRun(s, id(), 1, "2026-01-31", 1, 0, 0, 0)}
		}, "lease_run_in_order"},
		"a line posted twice": {func(s leaseRunScene) []stmt {
			later := id()
			return []stmt{leaseRun(s, s.run, 1, "2026-01-31", 1, 1, 5, 90), leaseRunPosting(s, s.run, 1, 2, 5, 90, "1"),
				leaseRun(s, later, 3, "2026-03-31", 1, 1, 5, 90), leaseRunPosting(s, later, 2, 3, 5, 90, "2")}
		}, "lease_line_posted_once"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			s := leaseRunScene{tenant: id(), contract: id(), run: id()}
			failsWith(t, all(tx, append(s.setup(), c.run(s)...)...), c.want)
		})
	}
}

// TestLeaseRunRevert (T88c, migration 000331's down): the revert is
// refused, before it changes anything, while a lease run exists, whose
// lines the next run would post again.
func TestLeaseRunRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000331_lease_run").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := leaseRunScene{tenant: id(), contract: id(), run: id()}
	if err := all(tx, leaseRun(s, s.run, 1, "2026-01-31", 0, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would post their lines again")
}
