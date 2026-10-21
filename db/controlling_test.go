package db

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Controlling rules (T56c, migration 000145): what the database keeps of
// the configuration's cost elements, categories, center accounting and
// default assignments.

func TestControllingRules(t *testing.T) {
	need(t)
	f := newCenters(t)
	otherCC := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, $2, $3, '1000', 'CC', 'J', 'pending', $4, $4)`, f.tenant, otherCC, f.other, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.company_controlling (tenant_id, legal_entity_id, cost_center_accounting_from, profit_center_accounting_from,
			default_profit_center_id, created_by, updated_by) VALUES ($1, $2, 2026, 2026, $3, $4, $4)`, f.tenant, f.entity, f.pc, f.actor)
	})
	const assignment = `INSERT INTO ledger.default_assignment (tenant_id, legal_entity_id, account_id, cost_center_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5)`
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a primary cost on a balance sheet account": {`UPDATE ledger.gl_account SET cost_element = 'primary_cost', version = version + 1 WHERE id = $1`,
			[]any{f.bank}, "gl_account_cost_element"},
		"a revenue element on an expense": {`UPDATE ledger.gl_account SET cost_element = 'revenue', version = version + 1 WHERE id = $1`,
			[]any{f.expense}, "gl_account_cost_element"},
		"a secondary cost element": {`UPDATE ledger.gl_account SET cost_element = 'secondary', version = version + 1 WHERE id = $1`,
			[]any{f.expense}, "gl_account_cost_element"},
		"a category code SAP cannot hold": {`INSERT INTO ledger.cost_center_category (tenant_id, code, name, created_by, updated_by) VALUES ($1, 'sales', 'S', $2, $2)`,
			[]any{f.tenant, f.actor}, "cost_center_category_code_check"},
		"a category without a name": {`INSERT INTO ledger.cost_center_category (tenant_id, code, name, created_by, updated_by) VALUES ($1, 'SALES', '', $2, $2)`,
			[]any{f.tenant, f.actor}, "cost_center_category_name_check"},
		"a period of an unknown category": {`INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, category,
			change_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $3, '2027-01-01', 'SALES', $4, $5, $5)`,
			[]any{f.tenant, otherCC, f.other, f.ccChange, f.actor}, "cost_center_period_category"},
		"a category deleted": {`DELETE FROM ledger.cost_center_category WHERE code = 'ADMIN'`, nil, "permission denied"},
		"a default profit center of another company": {`INSERT INTO ledger.company_controlling (tenant_id, legal_entity_id, default_profit_center_id,
			created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`, []any{f.tenant, f.other, f.pc, f.actor}, "company_controlling_default_profit_center"},
		"profit center accounting without a default": {`INSERT INTO ledger.company_controlling (tenant_id, legal_entity_id, profit_center_accounting_from,
			created_by, updated_by) VALUES ($1, $2, 2026, $3, $3)`, []any{f.tenant, f.other, f.actor}, "company_controlling_needs_default"},
		"cost center accounting turned off": {`UPDATE ledger.company_controlling SET cost_center_accounting_from = NULL, version = version + 1
			WHERE legal_entity_id = $1`, []any{f.entity}, "center accounting stays on"},
		"profit center accounting moved to another year": {`UPDATE ledger.company_controlling SET profit_center_accounting_from = 2027, version = version + 1
			WHERE legal_entity_id = $1`, []any{f.entity}, "center accounting stays on"},
		"a default cost center of another company": {assignment, []any{f.tenant, f.entity, f.expense, otherCC, f.actor},
			"default_assignment_cost_center_company"},
		"a default for an account the company has not set up": {assignment, []any{f.tenant, f.other, f.expense, otherCC, f.actor},
			"default_assignment_tenant_id_legal_entity_id_account_id_fkey"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.try(t, c.sql, c.args...), c.want)
		})
	}
	t.Run("a default cost center of the company, deactivated", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, assignment, f.tenant, f.entity, f.expense, f.cc, f.actor); err != nil {
				return err
			}
			return exec(tx, `UPDATE ledger.default_assignment SET is_active = false, version = version + 1 WHERE account_id = $1`, f.expense)
		})
	})
	t.Run("center accounting turned on in another company", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO ledger.company_controlling (tenant_id, legal_entity_id, created_by, updated_by) VALUES ($1, $2, $3, $3)`,
				f.tenant, f.other, f.actor); err != nil {
				return err
			}
			return exec(tx, `UPDATE ledger.company_controlling SET cost_center_accounting_from = 2027, version = version + 1 WHERE legal_entity_id = $1`, f.other)
		})
	})
	t.Run("an account inserted without a cost element has none", func(t *testing.T) {
		var got string
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT cost_element FROM ledger.gl_account WHERE id = $1`, f.expense).Scan(&got)
		})
		if got != "none" {
			t.Errorf("got %s, want none (setup writes the type's default)", got)
		}
	})
}

// TestControllingBackfill: accounts that exist before migration 000145 take
// their type's cost element, and the categories cost center periods have
// become categories named by their code, one per tenant and code.
func TestControllingBackfill(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	all := Migrations()
	at := slices.IndexFunc(all, func(m Migration) bool { return strings.HasSuffix(m.Name, "_controlling_rules") })
	if _, err := migrate(ctx, conn, all[:at]); err != nil {
		t.Fatal(err)
	}
	tenant, actor, chart, entity, change := id(), id(), id(), id(), id()
	cc1, cc2 := id(), id()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ledger.chart_of_accounts (tenant_id, id, code, name, created_by) VALUES ($1, $2, '1000', 'C', $3)`, []any{tenant, chart, actor}},
		{`INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, '520000', 'E', 'expense', 'active', $3, $3), ($1, gen_random_uuid(), $2, '400000', 'R', 'revenue', 'active', $3, $3),
			($1, gen_random_uuid(), $2, '100000', 'B', 'asset', 'active', $3, $3)`, []any{tenant, chart, actor}},
		{`INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			VALUES ($1, $2, $4, 'CC1', 'N', 'J', 'pending', $5, $5), ($1, $3, $4, 'CC2', 'N', 'J', 'pending', $5, $5)`, []any{tenant, cc1, cc2, entity, actor}},
		{`INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, kind, payload, reason, status, requested_by, applied_at, created_by, updated_by)
			VALUES ($1, $2, $3, 'create', '{}', 'r', 'applied', $4, now(), $4, $4)`, []any{tenant, change, cc1, actor}},
		{`INSERT INTO ledger.cost_center_period (tenant_id, id, cost_center_id, legal_entity_id, valid_from, valid_to, category, change_id, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), $2, $4, '2026-01-01', '2026-06-01', 'ADMIN', $5, $6, $6), ($1, gen_random_uuid(), $2, $4, '2026-06-01', NULL, 'SALES', $5, $6, $6),
			($1, gen_random_uuid(), $3, $4, '2026-01-01', NULL, 'ADMIN', $5, $6, $6)`, []any{tenant, cc1, cc2, entity, change, actor}},
	} {
		if _, err := conn.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("%s: %v", s.sql[:40], err)
		}
	}
	if _, err := migrate(ctx, conn, all[at:]); err != nil {
		t.Fatal(err)
	}
	for sql, want := range map[string]string{
		`SELECT string_agg(number || '=' || cost_element || '/v' || version, ' ' ORDER BY number) FROM ledger.gl_account WHERE tenant_id = $1 AND $2::uuid IS NOT NULL`:   "100000=none/v1 400000=revenue/v2 520000=primary_cost/v2",
		`SELECT string_agg(code || '=' || name || '/' || is_active || '/' || (created_by = $2), ' ' ORDER BY code) FROM ledger.cost_center_category WHERE tenant_id = $1`: "ADMIN=ADMIN/true/true SALES=SALES/true/true",
	} {
		var got string
		if err := conn.QueryRow(ctx, sql, tenant, actor).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("after the backfill: got %s, want %s", got, want)
		}
	}
}
