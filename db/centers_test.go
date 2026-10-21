package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Cost centers, profit centers and segments (T56a, migration 000128): the
// rules the database keeps for the centers, their periods and changes.

// centerFixture is a fixture with a second legal entity, a profit center
// assigned to the first, a cost center of the first and their create
// changes, all active, each in the leaf of its standard hierarchy (T56b).
type centerFixture struct {
	fixture
	other, pc, cc, pcChange, ccChange, rejected    uuid.UUID
	ccTree, pcTree, ccRoot, pcRoot, ccLeaf, pcLeaf uuid.UUID
}

func newCenters(t *testing.T) centerFixture {
	t.Helper()
	f := centerFixture{fixture: newFixture(t), other: id(), pc: id(), cc: id(), pcChange: id(), ccChange: id(), rejected: id(),
		ccTree: id(), pcTree: id(), ccRoot: id(), pcRoot: id(), ccLeaf: id(), pcLeaf: id()}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
				VALUES ($1, $2, '2000', 'Acme CA', 'CA', 'CAD', $3, 'active', $4, $4)`, []any{f.tenant, f.other, f.calendar, f.actor}},
			{`INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, 'PC', 'PC', 'J', 'active', $3, $3)`, []any{f.tenant, f.pc, f.actor}},
			{`INSERT INTO ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id, created_by) VALUES ($1, $2, $3, $4)`,
				[]any{f.tenant, f.pc, f.entity, f.actor}},
			{`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, $3, '1000', 'CC', 'J', 'active', $4, $4)`, []any{f.tenant, f.cc, f.entity, f.actor}},
			{`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, $3, 'R1', 'CC', 'J', 'pending', $4, $4)`, []any{f.tenant, f.rejected, f.entity, f.actor}},
			{`UPDATE ledger.cost_center SET status = 'rejected', version = version + 1 WHERE id = $1`, []any{f.rejected}},
			{`INSERT INTO ledger.center_change (tenant_id, id, profit_center_id, kind, payload, reason, status, requested_by, applied_at, created_by, updated_by)
				VALUES ($1, $2, $3, 'create', '{}', 'r', 'applied', $4, now(), $4, $4)`, []any{f.tenant, f.pcChange, f.pc, f.actor}},
			{`INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, kind, payload, reason, status, requested_by, applied_at, created_by, updated_by)
				VALUES ($1, $2, $3, 'create', '{}', 'r', 'applied', $4, now(), $4, $4)`, []any{f.tenant, f.ccChange, f.cc, f.actor}},
			{`INSERT INTO ledger.cost_center_category (tenant_id, code, name, created_by, updated_by) VALUES ($1, 'ADMIN', 'Administration', $2, $2)`,
				[]any{f.tenant, f.actor}},
			{`INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, category, profit_center_id, change_id, created_by, updated_by)
				VALUES ($1, gen_random_uuid(), $2, $3, '2026-01-01', 'ADMIN', $4, $5, $6, $6)`, []any{f.tenant, f.cc, f.entity, f.pc, f.ccChange, f.actor}},
			{`INSERT INTO ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, change_id, created_by, updated_by)
				VALUES ($1, gen_random_uuid(), $2, '2026-01-01', $3, $4, $4)`, []any{f.tenant, f.pc, f.pcChange, f.actor}},
			{hierarchy, []any{f.tenant, f.ccTree, "cost_center", "STD", true, f.actor}},
			{hierarchy, []any{f.tenant, f.pcTree, "profit_center", "STD", true, f.actor}},
			{node, []any{f.tenant, f.ccRoot, f.ccTree, nil, "STD", f.actor}},
			{node, []any{f.tenant, f.pcRoot, f.pcTree, nil, "STD", f.actor}},
			{node, []any{f.tenant, f.ccLeaf, f.ccTree, f.ccRoot, "LEAF", f.actor}},
			{node, []any{f.tenant, f.pcLeaf, f.pcTree, f.pcRoot, "LEAF", f.actor}},
			{member, []any{f.tenant, f.ccTree, "cost_center", f.ccLeaf, f.cc, nil, f.actor}},
			{member, []any{f.tenant, f.pcTree, "profit_center", f.pcLeaf, nil, f.pc, f.actor}},
		} {
			if err := exec(tx, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

// Statements that write hierarchies (T56b, migration 000128).
const (
	hierarchy = `INSERT INTO ledger.hierarchy (tenant_id, id, kind, code, name, is_standard, created_by, updated_by) VALUES ($1, $2, $3, $4, 'H', $5, $6, $6)`
	node      = `INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, parent_id, code, name, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'N', $6, $6)`
	member = `INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, cost_center_id, profit_center_id, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, $6, $7, $7)`
)

// try runs one statement as the application in the fixture's tenant.
func (f centerFixture) try(t *testing.T, sql string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
}

// ccPeriod inserts a period of the fixture's cost center.
const ccPeriod = `INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, valid_to, category, profit_center_id,
	change_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, 'ADMIN', $6, $7, $8, $8)`

func TestCenterMasterData(t *testing.T) {
	need(t)
	f := newCenters(t)
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a code SAP cannot hold": {`INSERT INTO ledger.segment (tenant_id, id, code, name, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'retail', 'R', $2, $2)`,
			[]any{f.tenant, f.actor}, "segment_code_check"},
		"a segment code twice": {`INSERT INTO ledger.segment (tenant_id, id, code, name, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'S1', 'R', $2, $2), ($1, gen_random_uuid(), 'S1', 'R', $2, $2)`, []any{f.tenant, f.actor}, "segment_code_once"},
		"a cost center code twice in a company": {`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, '1000', 'CC', 'J', 'pending', $3, $3)`, []any{f.tenant, f.entity, f.actor}, "cost_center_code_once"},
		"a profit center code twice": {`INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'PC', 'PC', 'J', 'pending', $2, $2)`, []any{f.tenant, f.actor}, "profit_center_code_once"},
		"a center's code changed": {`UPDATE ledger.cost_center SET code = '1001', version = version + 1 WHERE id = $1`, []any{f.cc}, "the code of a center is fixed"},
		"a cost center's company changed": {`UPDATE ledger.cost_center SET legal_entity_id = $2, version = version + 1 WHERE id = $1`, []any{f.cc, f.other},
			"the legal entity of a cost center is fixed"},
		"an active center rejected": {`UPDATE ledger.profit_center SET status = 'rejected', version = version + 1 WHERE id = $1`, []any{f.pc},
			"a center cannot go from active to rejected"},
		"a rejected center activated": {`UPDATE ledger.cost_center SET status = 'active', version = version + 1 WHERE id = $1`, []any{f.rejected},
			"a center cannot go from rejected to active"},
		"a company assignment deleted": {`DELETE FROM ledger.profit_center_company WHERE profit_center_id = $1`, []any{f.pc}, "permission denied"},
		"a profit center period ending before it starts": {`INSERT INTO ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, valid_to,
			change_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, '2025-06-01', '2025-05-01', $3, $4, $4)`, []any{f.tenant, f.pc, f.pcChange, f.actor},
			"profit_center_period_dates"},
		"an unknown kind of change": {`INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, kind, payload, reason, status, requested_by, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, 'delete', '{}', 'r', 'pending', $3, $3, $3)`, []any{f.tenant, f.cc, f.actor}, "center_change_kind_check"},
		"a company assignment removed": {`UPDATE ledger.profit_center_company SET legal_entity_id = $2 WHERE profit_center_id = $1`, []any{f.pc, f.other},
			"permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a rejected center's code used again", func(t *testing.T) {
		rejected := id()
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, $2, $3, '2000', 'CC', 'J', 'pending', $4, $4)`, f.tenant, rejected, f.entity, f.actor); err != nil {
				return err
			}
			if err := exec(tx, `UPDATE ledger.cost_center SET status = 'rejected', version = version + 1 WHERE id = $1`, rejected); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
				VALUES ($1, gen_random_uuid(), $2, '2000', 'CC', 'J', 'pending', $3, $3)`, f.tenant, f.entity, f.actor)
		})
	})
}

func TestCenterPeriods(t *testing.T) {
	need(t)
	f := newCenters(t)
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"overlapping an active period": {ccPeriod, []any{f.tenant, f.cc, f.entity, "2026-06-01", nil, nil, f.ccChange, f.actor}, "cost_center_period_overlap"},
		"ending before it starts": {ccPeriod, []any{f.tenant, f.cc, f.entity, "2025-06-01", "2025-06-01", nil, f.ccChange, f.actor},
			"cost_center_period_dates"},
		"naming another company's cost center": {ccPeriod, []any{f.tenant, f.cc, f.other, "2025-01-01", "2025-06-01", nil, f.ccChange, f.actor},
			"cost_center_period_company"},
		"a profit center the company is not assigned": {ccPeriod, []any{f.tenant, f.cc, f.entity, "2025-01-01", "2025-06-01", id(), f.ccChange, f.actor},
			"cost_center_period_profit_center_company"},
		"a profit center period overlapping": {`INSERT INTO ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, change_id, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, '2026-03-01', $3, $4, $4)`, []any{f.tenant, f.pc, f.pcChange, f.actor}, "profit_center_period_overlap"},
		"a period's attributes changed": {`UPDATE ledger.cost_center_period SET lock_plan = true, version = version + 1 WHERE cost_center_id = $1`, []any{f.cc},
			"a period is fixed"},
		"a period's validity changed": {`UPDATE ledger.profit_center_period SET valid_to = '2027-01-01', version = version + 1 WHERE profit_center_id = $1`,
			[]any{f.pc}, "a period is fixed"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("retired, then replaced by an overlapping period", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE ledger.cost_center_period SET retired_by_change_id = $2, version = version + 1 WHERE cost_center_id = $1`, f.cc, f.ccChange); err != nil {
				return err
			}
			return exec(tx, ccPeriod, f.tenant, f.cc, f.entity, "2026-01-01", "2026-06-01", f.pc, f.ccChange, f.actor)
		})
		failsWith(t, f.try(t, `UPDATE ledger.cost_center_period SET retired_by_change_id = $2, version = version + 1
			WHERE cost_center_id = $1 AND retired_by_change_id IS NOT NULL`, f.cc, f.pcChange), "it can only be retired, once")
	})
}

func TestCenterChanges(t *testing.T) {
	need(t)
	f := newCenters(t)
	change := `INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, profit_center_id, kind, payload, reason, status, requested_by, applied_at,
		created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '{}', 'r', $6, $7, CASE WHEN $6 = 'applied' THEN now() END, $7, $7)`
	for name, c := range map[string]struct {
		cc, pc       *uuid.UUID
		kind, status string
		want         string
	}{
		"for no center":                        {nil, nil, "lock", "pending", "center_change_one_center"},
		"for both centers":                     {&f.cc, &f.pc, "lock", "pending", "center_change_one_center"},
		"assigning a cost center to companies": {&f.cc, nil, "assign", "pending", "center_change_assign"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, change, f.tenant, id(), c.cc, c.pc, c.kind, c.status, f.actor), c.want)
		})
	}
	t.Run("applied without a time", func(t *testing.T) {
		failsWith(t, f.try(t, `UPDATE ledger.center_change SET applied_at = NULL, version = version + 1 WHERE id = $1`, f.ccChange), "center_change_applied")
	})
	pending := id()
	if err := f.try(t, change, f.tenant, pending, f.cc, nil, "lock", "pending", f.actor); err != nil {
		t.Fatalf("a pending change: got %v, want none", err)
	}
	if err := f.try(t, `UPDATE ledger.center_change SET workflow_request_id = gen_random_uuid(), version = version + 1 WHERE id = $1`, pending); err != nil {
		t.Fatalf("naming the approval request: got %v, want none", err)
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a second pending change": {change, []any{f.tenant, id(), f.cc, nil, "move", "pending", f.actor}, "center_change_one_pending"},
		"what it asks for changed": {`UPDATE ledger.center_change SET payload = '{"x": 1}', version = version + 1 WHERE id = $1`, []any{pending},
			"what a change asks for is fixed"},
		"its approval request changed": {`UPDATE ledger.center_change SET workflow_request_id = gen_random_uuid(), version = version + 1 WHERE id = $1`,
			[]any{pending}, "the approval request of a change is fixed"},
		"decided twice": {`UPDATE ledger.center_change SET status = 'rejected', applied_at = NULL, version = version + 1 WHERE id = $1`, []any{f.ccChange},
			"a change cannot go from applied to rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
}
