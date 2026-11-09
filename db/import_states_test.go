package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// An import restores rows in any lifecycle state (ADR-0029, migrations
// 000230 and 000234, an accepted intercompany difference): a row past its first state, or one written while its parent
// was in a state it has left, is inserted only while app.importing names
// the row's own tenant; every other insert is refused as before. A
// carry-forward run, never deleted, goes with its tenant's erasure alone.

// importCase writes, in before, rows reached through their transitions,
// then, in row, the rows a normal insert refuses with want.
type importCase struct {
	name   string
	before func(tx pgx.Tx, c carryForward) error // nil: nothing
	row    func(tx pgx.Tx, c carryForward) error
	want   string
}

const (
	importedClose = `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version, status,
		created_by, updated_by) VALUES ($1, $2, $3, 2026, 1, '2026-01-31', 1, $4, $5, $5)`
	importedRun = `INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start, period_end,
		config_version, group_currency, rate_type, status, operation_id, computed_at, finalized_by, finalized_at, created_by, updated_by)
		VALUES ($1, $2, 'GROUP', $3, ARRAY[$3, gen_random_uuid()]::uuid[], 2026, 1, '2026-01-01', '2026-01-31', 1, 'EUR', 'closing', $4,
		gen_random_uuid(), $5::timestamptz, $6::uuid, $5::timestamptz, $7, $7)`
	importedMember = `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, local_currency, closing_rate)
		VALUES ($1, $2, $3, '1000', 'parent', 'USD', 1)`
	importedBalance = `INSERT INTO consolidation.run_balance (tenant_id, run_id, legal_entity_id, line_no, group_account, kind, amount_gc)
		VALUES ($1, $2, $3, 1, 'G3900', 'cta', 5)`
	importedDifference = `INSERT INTO consolidation.run_difference (tenant_id, id, run_id, line_no, elimination_code, legal_entity_id, partner_id, status,
		amount, partner_amount, difference, tolerance, accepted_by, accepted_at, reason, version)
		VALUES ($1, gen_random_uuid(), $2, 1, 'IC_BALANCES', $3, gen_random_uuid(), 'accepted', -2800, 2700, -100, 10, $4, now(), 'Rates', 2)`
)

// importCases are the rows migration 000230 lets an import restore.
func importCases() []importCase {
	closeID, runID, parked := uuid.MustParse("0192a000-0000-7000-8000-00000000c105"), uuid.MustParse("0192a000-0000-7000-8000-00000000c0a1"),
		uuid.MustParse("0192a000-0000-7000-8000-00000000fa4c")
	finalRun := func(tx pgx.Tx, c carryForward, member bool) error {
		stmts := []stmt{step(importedRun, c.tenant, runID, c.entity, "queued", nil, nil, c.actor),
			step(`UPDATE consolidation.run SET status = 'running', version = version + 1`)}
		if member {
			stmts = append(stmts, step(importedMember, c.tenant, runID, c.entity))
		}
		return all(tx, append(stmts, step(`UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1`),
			step(`UPDATE consolidation.run SET status = 'final', finalized_by = updated_by, finalized_at = now(), version = version + 1`))...)
	}
	return []importCase{
		{name: "a signed-off close", row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedClose, c.tenant, closeID, c.entity, "signed_off", c.actor)
		}, want: "period_close_transition"},
		{name: "a check task done on its passing check", before: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedClose, c.tenant, closeID, c.entity, "open", c.actor)
		}, row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, `INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, status, completed_by, completed_at,
				checked_at, checked_by, check_passed, check_result, created_by, updated_by) VALUES ($1, $2, 'NP', 1, 'A check', 'check', 'no_parked_entries',
				'2026-02-03', 'done', $3, now(), now(), $3, true, '{}', $3, $3)`, c.tenant, closeID, c.actor)
		}, want: "a close task starts open"},
		{name: "a posted parked entry", row: func(tx pgx.Tx, c carryForward) error {
			_, err := c.parkedFor(tx, parked, "posted")
			return err
		}, want: "an entry is parked first"},
		{name: "a posted parked entry's line", before: func(tx pgx.Tx, c carryForward) error {
			je, err := c.parkedFor(tx, parked, "parked")
			if err != nil {
				return err
			}
			return exec(tx, `UPDATE ledger.parked_entry SET status = 'posted', journal_fiscal_year = 2026, journal_entry_id = $2, version = version + 1
				WHERE id = $1`, parked, je)
		}, row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, `INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount) VALUES ($1, $2, 1, $3, 100)`,
				c.tenant, parked, c.expense)
		}, want: "items of a posted ledger.parked_entry cannot change"},
		{name: "a final consolidation run", row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedRun, c.tenant, runID, c.entity, "final", "2026-02-01T00:00:00Z", c.actor, c.actor)
		}, want: "run_transition"},
		{name: "a final run's member", before: func(tx pgx.Tx, c carryForward) error {
			return finalRun(tx, c, false)
		}, row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedMember, c.tenant, runID, c.entity)
		}, want: "run_rows_while_running"},
		{name: "a final run's balance", before: func(tx pgx.Tx, c carryForward) error {
			return finalRun(tx, c, true)
		}, row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedBalance, c.tenant, runID, c.entity)
		}, want: "run_rows_while_running"},
		{name: "a final run's accepted difference", before: func(tx pgx.Tx, c carryForward) error {
			return finalRun(tx, c, true)
		}, row: func(tx pgx.Tx, c carryForward) error {
			return exec(tx, importedDifference, c.tenant, runID, c.entity, c.actor)
		}, want: "run_difference_transition"},
		{name: "a superseded carry-forward run, then its successor", row: func(tx pgx.Tx, c carryForward) error {
			old, next := id(), id()
			if err := exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
				retained_earnings_account_id, row_count, created_by, status, superseded_by_id, superseded_at) VALUES ($1, $2, $3, $4, 2026, 2027, 'all_years',
				$5, 0, $6, 'superseded', $7, now())`, c.tenant, old, c.entity, c.ledger, c.equity, c.actor, next); err != nil {
				return err
			}
			return c.insertRun(tx, next, 2026, nil, c.equity)
		}, want: "carry_forward_run_lifecycle"},
	}
}

// parkedFor writes a journal entry posted for parked entry pe, then pe,
// of c's person, in status: posted, naming the entry, or parked. It
// returns the journal entry's id.
func (c carryForward) parkedFor(tx pgx.Tx, pe uuid.UUID, status string) (uuid.UUID, error) {
	je, err := c.entryFor(tx, &pe, 5, "2026-05-04", line{account: c.expense, amount: 100}, line{account: c.bank, amount: -100})
	if err != nil {
		return je, err
	}
	var year *int
	var entry *uuid.UUID
	if status == "posted" {
		y := 2026
		year, entry = &y, &je
	}
	return je, exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date, currency,
		created_by_kind, owner_id, created_by, updated_by, status, journal_fiscal_year, journal_entry_id)
		VALUES ($1, $2, $3, $4, 'SA', '2026-05-04', '2026-05-04', 'USD', 'human', $5, $5, $5, $6, $7, $8)`,
		c.tenant, pe, c.entity, c.ledger, c.actor, status, year, entry)
}

// TestImportRestoresLifecycleStates: each row a normal insert refuses for
// its state is refused while app.importing names another tenant, and
// inserted while it names the row's own.
func TestImportRestoresLifecycleStates(t *testing.T) {
	need(t)
	for _, tc := range importCases() {
		for _, v := range []struct {
			name     string
			importer func(c carryForward) string // "": no import
			want     string
		}{
			{"inserted", func(carryForward) string { return "" }, tc.want},
			{"imported for another tenant", func(carryForward) string { return uuid.NewString() }, tc.want},
			{"imported", func(c carryForward) string { return c.tenant.String() }, ""},
		} {
			t.Run(tc.name+" "+v.name, func(t *testing.T) {
				c := newCarryForward(t)
				refused(t, asApp(t, c.tenant, func(tx pgx.Tx) error {
					if tc.before != nil {
						if err := tc.before(tx, c); err != nil {
							t.Fatalf("the rows before: %v", err)
						}
					}
					if importer := v.importer(c); importer != "" {
						if err := exec(tx, "SELECT set_config('app.importing', $1, true)", importer); err != nil {
							return err
						}
					}
					return tc.row(tx, c)
				}), v.want)
			})
		}
	}
}

// TestCarryForwardRunErasedWithItsTenant: a carry-forward run is deleted
// only by its tenant's erasure (app.erasing naming the tenant), as
// platform.forbid_mutation lets it delete an insert-only row.
func TestCarryForwardRunErasedWithItsTenant(t *testing.T) {
	need(t)
	for _, tc := range []struct {
		name    string
		erasing func(c carryForward) string // "": no erasure
		want    string
	}{
		{"deleted", func(carryForward) string { return "" }, "carry_forward_run_fixed"},
		{"deleted erasing another tenant", func(carryForward) string { return uuid.NewString() }, "carry_forward_run_fixed"},
		{"erased with its tenant", func(c carryForward) string { return c.tenant.String() }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCarryForward(t)
			ctx := context.Background()
			tx, err := testDB.Begin(ctx) // as the owner, which erase_tenant runs as: erp_app may not delete at all
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // the test's work is read from the error; nothing is kept
			if erasing := tc.erasing(c); erasing != "" {
				if err := exec(tx, "SELECT set_config('app.erasing', $1, true)", erasing); err != nil {
					t.Fatal(err)
				}
			}
			refused(t, exec(tx, `DELETE FROM ledger.carry_forward_run WHERE id = $1`, c.run), tc.want)
		})
	}
}
