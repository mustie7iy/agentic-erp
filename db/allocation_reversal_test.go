package db

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Cycles by fixed amounts, runs reversed and run again (T60k, migrations
// 000375-000377; ADR-0064 As built T60k).

const (
	fixedCycleSQL = `INSERT INTO controlling.allocation_cycle (tenant_id, id, legal_entity_id, code, name, kind, sender_cost_center_id, assessment_account_id,
		share_basis, created_by, updated_by) VALUES ($1, $2, $3, $4, 'Cycle', $5, $6, $7, 'fixed_amounts', $8, $8)`
	amountReceiverSQL = `INSERT INTO controlling.allocation_receiver (tenant_id, cycle_id, position, cost_center_id, share_bp, amount)
		VALUES ($1, $2, $3, $4, $5, $6)`
	attemptRunSQL = `INSERT INTO controlling.allocation_run (tenant_id, id, cycle_id, legal_entity_id, fiscal_year, fiscal_period, posting_date,
		journal_entry_id, document_number, amount, account_count, created_by, attempt) VALUES ($1, $2, $3, $4, 2026, $5, '2026-05-31', $6, '1', 100, 0, $7, $8)`
	runReversalSQL = `INSERT INTO controlling.allocation_run_reversal (tenant_id, run_id, journal_entry_id, document_number, created_by) VALUES ($1, $2, $3, $4, $5)`
)

func TestAllocationFixedAmountRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	// write writes a cycle by fixed amounts of the kind, its receivers with
	// the shares and amounts given (nil for none), in one transaction.
	write := func(t *testing.T, kind string, receivers ...[2]any) error {
		t.Helper()
		cycle := id()
		var account any
		if kind == "assessment" {
			account = id()
		}
		return asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, fixedCycleSQL, f.tenant, cycle, f.entity, "F"+strings.ToUpper(cycle.String()[30:]), kind, id(), account, f.actor); err != nil {
				return err
			}
			for i, r := range receivers {
				if err := exec(tx, amountReceiverSQL, f.tenant, cycle, i+1, id(), r[0], r[1]); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := write(t, "assessment", [2]any{nil, 25000}, [2]any{nil, 15000}); err != nil {
		t.Fatalf("an assessment by fixed amounts: %v", err)
	}
	for name, c := range map[string]struct {
		kind      string
		receivers [][2]any
		want      string
	}{
		"a distribution by fixed amounts":      {"distribution", [][2]any{{nil, 100}}, "allocation_cycle_fixed_amounts"},
		"a receiver without its amount":        {"assessment", [][2]any{{nil, 100}, {nil, nil}}, "each has an amount exactly by fixed amounts"},
		"a receiver with a share":              {"assessment", [][2]any{{10000, 100}}, "none has a share of its own"},
		"an amount of nothing":                 {"assessment", [][2]any{{nil, 0}}, "allocation_receiver_amount"},
		"a cycle by fixed amounts without one": {"assessment", nil, "so it has receivers"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, write(t, c.kind, c.receivers...), c.want)
		})
	}
	t.Run("an amount on fixed percentages", func(t *testing.T) {
		cycle := id()
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, allocationCycleSQL, f.tenant, cycle, f.entity, "PCT", "distribution", id(), f.actor); err != nil {
				return err
			}
			return exec(tx, amountReceiverSQL, f.tenant, cycle, 1, id(), 10000, 500)
		}), "or a receiver has none or an amount")
	})
}

func TestAllocationRunReversalRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	cycle, sender, first, second := id(), id(), id(), id()
	if err := f.cycleWith(t, cycle, "SHARED", "distribution", sender, receiverShare{id(), 10000}); err != nil {
		t.Fatal(err)
	}
	entry := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, attemptRunSQL, f.tenant, first, cycle, f.entity, 5, id(), f.actor, 1); err != nil {
			return err
		}
		return exec(tx, runReversalSQL, f.tenant, first, entry, "100000002", f.actor)
	})
	try := func(sql string, args ...any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a run reversed twice":              {runReversalSQL, []any{f.tenant, first, id(), "1", f.actor}, "allocation_run_reversal_pkey"},
		"a reversal of no run":              {runReversalSQL, []any{f.tenant, id(), id(), "1", f.actor}, "allocation_run_reversal_run"},
		"a reversal without its number":     {runReversalSQL, []any{f.tenant, first, id(), "", f.actor}, "allocation_run_reversal_number"},
		"a reversal changed":                {`UPDATE controlling.allocation_run_reversal SET document_number = '9' WHERE run_id = $1`, []any{first}, "permission denied"},
		"a reversal deleted":                {`DELETE FROM controlling.allocation_run_reversal WHERE run_id = $1`, []any{first}, "permission denied"},
		"the reversed attempt again":        {attemptRunSQL, []any{f.tenant, id(), cycle, f.entity, 5, id(), f.actor, 1}, "allocation_run_attempt_once"},
		"an attempt beyond the next":        {attemptRunSQL, []any{f.tenant, id(), cycle, f.entity, 5, id(), f.actor, 3}, "after 1 earlier runs"},
		"a later attempt of a fresh period": {attemptRunSQL, []any{f.tenant, id(), cycle, f.entity, 6, id(), f.actor, 2}, "after 0 earlier runs"},
		"an attempt of nothing":             {attemptRunSQL, []any{f.tenant, id(), cycle, f.entity, 6, id(), f.actor, 0}, "is attempt 0"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, try(c.sql, c.args...), c.want)
		})
	}
	// The period is run again: attempt 2, in force, after which a third
	// attempt waits for its reversal.
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, attemptRunSQL, f.tenant, second, cycle, f.entity, 5, id(), f.actor, 2)
	})
	t.Run("an attempt while one is in force", func(t *testing.T) {
		failsWith(t, try(attemptRunSQL, f.tenant, id(), cycle, f.entity, 5, id(), f.actor, 3), "1 of them in force")
	})
	t.Run("an entry reversing two runs", func(t *testing.T) {
		failsWith(t, try(runReversalSQL, f.tenant, second, entry, "1", f.actor), "allocation_run_reversal_entry_once")
	})
	t.Run("the trigger holds for the owner", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, asOwner(func(tx pgx.Tx) error {
			return exec(tx, attemptRunSQL, f.tenant, id(), cycle, f.entity, 5, id(), f.actor, 3)
		})), "1 of them in force")
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM controlling.allocation_run_reversal`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("reversals another tenant sees: got %d, want 0", n)
		}
	})
}
