package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestDunningRunCancellation (T76i, migration 000396; ADR-0065 As built):
// a confirmed run moves to cancelled once, with a reason of 1 to 200
// characters that is not white space alone, and keeps its company, key
// date and totals; a proposed run is not cancelled, a cancelled one never
// changes, and only a cancelled one has a reason.
func TestDunningRunCancellation(t *testing.T) {
	need(t)
	const confirm = `UPDATE collections.dunning_run SET status = 'confirmed', version = version + 1 WHERE id = $1`
	const cancel = `UPDATE collections.dunning_run SET status = 'cancelled', cancellation_reason = $2, version = version + 1 WHERE id = $1`
	cases := []struct {
		name  string
		steps func(s chargeScene) []stmt
		want  string // empty: allowed
	}{
		{"a confirmed run cancelled", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, "Run in error")}
		}, ""},
		{"a reason of 200 characters", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, strings.Repeat("r", 200))}
		}, ""},
		{"a proposed run cancelled", func(s chargeScene) []stmt {
			return []stmt{step(cancel, s.run, "Run in error")}
		}, "or a confirmed one by being cancelled"},
		{"a run cancelled twice", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, "Run in error"), step(cancel, s.run, "Again")}
		}, "or a confirmed one by being cancelled"},
		{"a cancelled run confirmed again", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, "Run in error"),
				step(`UPDATE collections.dunning_run SET status = 'confirmed', cancellation_reason = NULL WHERE id = $1`, s.run)}
		}, "or a confirmed one by being cancelled"},
		{"a run cancelled without a reason", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(`UPDATE collections.dunning_run SET status = 'cancelled', version = version + 1 WHERE id = $1`, s.run)}
		}, "dunning_run_cancelled"},
		{"a reason of white space", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, "  ")}
		}, "dunning_run_cancelled"},
		{"a reason of 201 characters", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run), step(cancel, s.run, strings.Repeat("r", 201))}
		}, "dunning_run_cancelled"},
		{"a confirmation given a reason", func(s chargeScene) []stmt {
			return []stmt{step(`UPDATE collections.dunning_run SET status = 'confirmed', cancellation_reason = 'x', version = version + 1 WHERE id = $1`, s.run)}
		}, "dunning_run_cancelled"},
		{"a run's key date changed with its cancellation", func(s chargeScene) []stmt {
			return []stmt{step(confirm, s.run),
				step(`UPDATE collections.dunning_run SET status = 'cancelled', cancellation_reason = 'x', key_date = '2026-07-31' WHERE id = $1`, s.run)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := chargeScene{tenant: id(), run: id(), notice: id(), run2: id(), notice2: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := s.charged(tx); err != nil {
					t.Fatalf("the runs: got %v, want no error", err)
				}
				return all(tx, c.steps(s)...)
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

// TestDunningDebitMemoReversal (T76i, migration 000396): a debit memo
// another context entered from its document (a dunning notice) is
// reversed; a credit memo so entered still is not (TestCustomerMemoReversal).
func TestDunningDebitMemoReversal(t *testing.T) {
	need(t)
	tenant, memo := id(), id()
	err := asApp(t, tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
				currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, source_document_id, source_document_number,
				posting_date, payment_term_code, net_due_date, journal_entry_year, journal_entry_id)
				VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'debit_memo', '2026-03-10', 'EUR', 1500, 0, 1500, 'posted', $1, $1, $3, $4,
				'2026-03-12', 'NT30', '2026-04-09', 2026, gen_random_uuid())`, tenant, memo, id(), "notice"),
			step(`UPDATE receivables.customer_invoice SET status = 'reversed', reversal_date = '2026-03-12', reversal_entry_year = 2026,
				reversal_entry_id = gen_random_uuid(), reversal_reason = 'Run in error', version = version + 1 WHERE id = $1`, memo))
	})
	if err != nil {
		t.Fatalf("a dunning notice's debit memo reversed: got %v, want no error", err)
	}
}

// TestDunningCancelRevert (T76i, migration 000396's down; T76h, 000395's):
// the reverts are refused while a run is cancelled or a charge exists,
// what the previous versions cannot read.
func TestDunningCancelRevert(t *testing.T) {
	need(t)
	for _, c := range []struct {
		migration, extra, want string
	}{
		{"000396_dunning_run_cancel", `UPDATE collections.dunning_run SET status = 'cancelled', cancellation_reason = 'x', version = version + 1 WHERE id = $1`,
			"000396 cannot be reverted"},
		{"000395_dunning_charge_posting", `INSERT INTO collections.dunning_charge (tenant_id, dunning_notice_id, dunning_run_id, customer_invoice_id,
			customer_invoice_number, journal_entry_year, journal_entry_id, posting_date)
			SELECT tenant_id, id, dunning_run_id, gen_random_uuid(), 'CI1', 2026, gen_random_uuid(), '2026-07-01' FROM collections.dunning_notice
			WHERE dunning_run_id = $1`, "reverting would lose which debit memo charges which notice"},
	} {
		t.Run(c.migration, func(t *testing.T) {
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
			if err := all(tx, step(`UPDATE collections.dunning_run SET status = 'confirmed', version = version + 1 WHERE id = $1`, s.run),
				step(c.extra, s.run)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, migrationNamed(t, c.migration).Down), c.want)
		})
	}
}
