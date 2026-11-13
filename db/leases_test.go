package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLeasePermissions (T88a, migration 000308; ADR-0073 §6): the
// permissions of lease contracts are seeded and are no approval
// permissions.
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
	if got, want := strings.Join(codes, " "), "leases.contract.read leases.contract.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestLeaseContractRules (T88a, migration 000308; ADR-0073 §3-§4): a
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
		{"created at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
					payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, version, created_by, updated_by)
				VALUES ($1, $2, 'LC1', $1, $1, '2026-01-01', 12, 100, 'EUR', 'monthly', 'advance', 5, $1, 'draft', 0, $1, $1)`, s.tenant, s.contract)}
		}, "contract_version_check"},
		{"a contract changed", func(s scene) []stmt {
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
