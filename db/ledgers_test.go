package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// Parallel ledgers (T52a, migration 000091): 0L is the leading ledger, a
// company with ledgers keeps exactly one leading ledger, assignments and
// members are never taken back, and a group's representative is a member,
// the leading ledger when it is one, and never changes.
func TestParallelLedgerRules(t *testing.T) {
	need(t)
	const (
		local     = `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1, $2, '2L', 'Local', false, 'HGB', $3)`
		assign    = `INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by) VALUES ($1, $2, $3, $4, $5)`
		group     = `INSERT INTO ledger.ledger_group (tenant_id, id, code, name, representative_ledger_id, created_by, updated_by) VALUES ($1, $2, $3, 'Group', $4, $5, $5)`
		member    = `INSERT INTO ledger.ledger_group_member (tenant_id, group_id, ledger_id, created_by) VALUES ($1, $2, $3, $4)`
		groupCode = "G1"
	)
	for _, c := range []struct {
		name string
		run  func(tx pgx.Tx, f fixture) error
		want string // empty: allowed
	}{
		{"both ledgers of a company", func(tx pgx.Tx, f fixture) error {
			l := id()
			return all(tx, step(local, f.tenant, l, f.actor), step(assign, f.tenant, f.entity, f.ledger, true, f.actor), step(assign, f.tenant, f.entity, l, false, f.actor))
		}, ""},
		{"a parallel ledger without the leading one", func(tx pgx.Tx, f fixture) error {
			l := id()
			return all(tx, step(local, f.tenant, l, f.actor), step(assign, f.tenant, f.entity, l, false, f.actor))
		}, "has ledgers but not the leading ledger"},
		{"a parallel ledger assigned as leading", func(tx pgx.Tx, f fixture) error {
			l := id()
			return all(tx, step(local, f.tenant, l, f.actor), step(assign, f.tenant, f.entity, l, true, f.actor))
		}, "company_ledger_ledger_fkey"},
		{"a ledger renamed", func(tx pgx.Tx, f fixture) error {
			return exec(tx, `UPDATE ledger.ledger SET name = 'Renamed', version = version + 1 WHERE id = $1`, f.ledger)
		}, ""},
		{"a second ledger leading", func(tx pgx.Tx, f fixture) error {
			return exec(tx, `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1, $2, '2L', 'x', true, 'HGB', $3)`,
				f.tenant, id(), f.actor)
		}, "ledger_leading_is_0l"},
		{"a ledger's code changed", func(tx pgx.Tx, f fixture) error {
			return exec(tx, `UPDATE ledger.ledger SET code = '1L' WHERE id = $1`, f.ledger)
		}, "permission denied"},
		{"an assignment taken back", func(tx pgx.Tx, f fixture) error {
			if err := exec(tx, assign, f.tenant, f.entity, f.ledger, true, f.actor); err != nil {
				return err
			}
			return exec(tx, `UPDATE ledger.company_ledger SET created_by = $1`, f.actor)
		}, "permission denied"},
		{"a group of both, led by 0L", func(tx pgx.Tx, f fixture) error {
			l, g := id(), id()
			return all(tx, step(local, f.tenant, l, f.actor), step(group, f.tenant, g, groupCode, f.ledger, f.actor),
				step(member, f.tenant, g, f.ledger, f.actor), step(member, f.tenant, g, l, f.actor))
		}, ""},
		{"a representative not a member", func(tx pgx.Tx, f fixture) error {
			l, g := id(), id()
			return all(tx, step(local, f.tenant, l, f.actor), step(group, f.tenant, g, groupCode, f.ledger, f.actor), step(member, f.tenant, g, l, f.actor))
		}, "ledger_group_representative_member"},
		{"a group with 0L represented by another", func(tx pgx.Tx, f fixture) error {
			l, g := id(), id()
			return all(tx, step(local, f.tenant, l, f.actor), step(group, f.tenant, g, groupCode, l, f.actor),
				step(member, f.tenant, g, f.ledger, f.actor), step(member, f.tenant, g, l, f.actor))
		}, "must be its representative"},
		{"a group code", func(tx pgx.Tx, f fixture) error {
			return exec(tx, group, f.tenant, id(), "g1", f.ledger, f.actor)
		}, "ledger_group_code_check"},
		{"a group renamed", func(tx pgx.Tx, f fixture) error {
			g := id()
			return all(tx, step(group, f.tenant, g, groupCode, f.ledger, f.actor), step(member, f.tenant, g, f.ledger, f.actor),
				step(`UPDATE ledger.ledger_group SET name = 'Renamed', version = version + 1 WHERE id = $1`, g))
		}, ""},
		{"a group's representative changed", func(tx pgx.Tx, f fixture) error {
			l, g := id(), id()
			return all(tx, step(local, f.tenant, l, f.actor), step(group, f.tenant, g, groupCode, l, f.actor), step(member, f.tenant, g, l, f.actor),
				step(member, f.tenant, g, f.ledger, f.actor), step(`UPDATE ledger.ledger_group SET representative_ledger_id = $1 WHERE id = $2`, f.ledger, g))
		}, "only the name of a ledger group changes"},
		{"a member taken out", func(tx pgx.Tx, f fixture) error {
			g := id()
			return all(tx, step(group, f.tenant, g, groupCode, f.ledger, f.actor), step(member, f.tenant, g, f.ledger, f.actor),
				step(`UPDATE ledger.ledger_group_member SET created_by = $1`, f.actor))
		}, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return c.run(tx, f) })
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

type stmt struct {
	sql  string
	args []any
}

func step(sql string, args ...any) stmt { return stmt{sql, args} }

// all runs the statements in order, stopping at the first that fails.
func all(tx pgx.Tx, stmts ...stmt) error {
	for _, st := range stmts {
		if err := exec(tx, st.sql, st.args...); err != nil {
			return err
		}
	}
	return nil
}
