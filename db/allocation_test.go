package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Allocation cycles and their runs (T60f, migration 000276; ADR-0064 §5):
// what the database keeps of a cycle, its receivers' shares and its runs.

const (
	allocationCycleSQL = `INSERT INTO controlling.allocation_cycle (tenant_id, id, legal_entity_id, code, name, kind, sender_cost_center_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'Cycle', $5, $6, $7, $7)`
	allocationReceiverSQL = `INSERT INTO controlling.allocation_receiver (tenant_id, cycle_id, position, cost_center_id, share_bp) VALUES ($1, $2, $3, $4, $5)`
	allocationRunSQL      = `INSERT INTO controlling.allocation_run (tenant_id, id, cycle_id, legal_entity_id, fiscal_year, fiscal_period, posting_date,
		journal_entry_id, document_number, amount, account_count, created_by) VALUES ($1, $2, $3, $4, 2026, $5, '2026-05-31', $6, $7, $8, $9, $10)`
)

// receiverShare is a receiver of a cycle written by cycleWith.
type receiverShare struct {
	center uuid.UUID
	bp     int
}

// cycleWith writes a cycle of the fixture's company with the sender and
// the receivers in one transaction, as the application does.
func (f fixture) cycleWith(t *testing.T, cycle uuid.UUID, code, kind string, sender uuid.UUID, receivers ...receiverShare) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, allocationCycleSQL, f.tenant, cycle, f.entity, code, kind, sender, f.actor); err != nil {
			return err
		}
		for i, r := range receivers {
			if err := exec(tx, allocationReceiverSQL, f.tenant, cycle, i+1, r.center, r.bp); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestAllocationRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	cycle, sender, a, b, entry := id(), id(), id(), id(), id()
	if err := f.cycleWith(t, cycle, "SHARED", "distribution", sender, receiverShare{a, 6000}, receiverShare{b, 4000}); err != nil {
		t.Fatal(err)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, allocationRunSQL, f.tenant, id(), cycle, f.entity, 5, entry, "100000001", 15001, 2, f.actor)
	})
	t.Run("a cycle and its receivers", func(t *testing.T) {
		for name, c := range map[string]struct {
			code, kind string
			sender     uuid.UUID
			receivers  []receiverShare
			want       string
		}{
			"shares short of the whole":   {"X", "distribution", sender, []receiverShare{{a, 6000}, {b, 3999}}, "add up to 9999 basis points"},
			"shares beyond the whole":     {"X", "distribution", sender, []receiverShare{{a, 6000}, {b, 4001}}, "add up to 10001 basis points"},
			"a cycle without a receiver":  {"X", "distribution", sender, nil, "add up to 0 basis points"},
			"the sender as a receiver":    {"X", "distribution", sender, []receiverShare{{a, 6000}, {sender, 4000}}, "names its sender as a receiver"},
			"a receiver twice":            {"X", "distribution", sender, []receiverShare{{a, 6000}, {a, 4000}}, "allocation_receiver_once"},
			"a share of nothing":          {"X", "distribution", sender, []receiverShare{{a, 10000}, {b, 0}}, "allocation_receiver_share_bp_check"},
			"a share beyond the whole":    {"X", "distribution", sender, []receiverShare{{a, 10001}}, "allocation_receiver_share_bp_check"},
			"a kind that is none":         {"X", "assessment", sender, []receiverShare{{a, 10000}}, "allocation_cycle_kind_check"},
			"a code the form cannot hold": {"shared", "distribution", sender, []receiverShare{{a, 10000}}, "allocation_cycle_code_check"},
			"a code twice in a company":   {"SHARED", "distribution", sender, []receiverShare{{a, 10000}}, "allocation_cycle_code_once"},
		} {
			t.Run(name, func(t *testing.T) {
				failsWith(t, f.cycleWith(t, id(), c.code, c.kind, c.sender, c.receivers...), c.want)
			})
		}
	})
	try := func(sql string, args ...any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a receiver added to a whole cycle": {allocationReceiverSQL, []any{f.tenant, cycle, 3, id(), 1}, "add up to 10001 basis points"},
		"a receiver of no cycle":            {allocationReceiverSQL, []any{f.tenant, id(), 1, a, 10000}, "allocation_receiver_cycle"},
		"a receiver's share changed":        {`UPDATE controlling.allocation_receiver SET share_bp = 1 WHERE cycle_id = $1`, []any{cycle}, "permission denied"},
		"a receiver deleted":                {`DELETE FROM controlling.allocation_receiver WHERE cycle_id = $1`, []any{cycle}, "permission denied"},
		"a cycle's sender changed": {`UPDATE controlling.allocation_cycle SET sender_cost_center_id = $2, version = version + 1 WHERE id = $1`, []any{cycle, id()},
			"keeps its company, code, kind and sender"},
		"a cycle's code changed": {`UPDATE controlling.allocation_cycle SET code = 'OTHER', version = version + 1 WHERE id = $1`, []any{cycle},
			"keeps its company, code, kind and sender"},
		"a cycle deleted":                {`DELETE FROM controlling.allocation_cycle WHERE id = $1`, []any{cycle}, "permission denied"},
		"a second run of a period":       {allocationRunSQL, []any{f.tenant, id(), cycle, f.entity, 5, id(), "2", 100, 1, f.actor}, "allocation_run_once"},
		"an entry of two runs":           {allocationRunSQL, []any{f.tenant, id(), cycle, f.entity, 6, entry, "2", 100, 1, f.actor}, "allocation_run_entry_once"},
		"a run of no cycle":              {allocationRunSQL, []any{f.tenant, id(), id(), f.entity, 6, id(), "2", 100, 1, f.actor}, "allocation_run_cycle"},
		"a run in another company":       {allocationRunSQL, []any{f.tenant, id(), cycle, id(), 6, id(), "2", 100, 1, f.actor}, "allocation_run_cycle"},
		"a run of no account":            {allocationRunSQL, []any{f.tenant, id(), cycle, f.entity, 6, id(), "2", 100, 0, f.actor}, "allocation_run_account_count_check"},
		"a run of a period that is none": {allocationRunSQL, []any{f.tenant, id(), cycle, f.entity, 17, id(), "2", 100, 1, f.actor}, "allocation_run_fiscal_period_check"},
		"a run changed":                  {`UPDATE controlling.allocation_run SET amount = 1 WHERE cycle_id = $1`, []any{cycle}, "permission denied"},
		"a run deleted":                  {`DELETE FROM controlling.allocation_run WHERE cycle_id = $1`, []any{cycle}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, try(c.sql, c.args...), c.want)
		})
	}
	t.Run("a cycle renamed, and run for another period", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE controlling.allocation_cycle SET name = 'Renamed', version = version + 1 WHERE id = $1`, cycle); err != nil {
				return err
			}
			return exec(tx, allocationRunSQL, f.tenant, id(), cycle, f.entity, 6, id(), "100000002", -100, 1, f.actor)
		})
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM controlling.allocation_cycle) + (SELECT count(*) FROM controlling.allocation_receiver)
				+ (SELECT count(*) FROM controlling.allocation_run)`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("cycles, receivers and runs another tenant sees: got %d, want 0", n)
		}
	})
}
