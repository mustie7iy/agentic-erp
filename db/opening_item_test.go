package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestOpeningItemRules: every rule the database enforces on the open
// vendor items of a migration's opening entries (000499; ADR-0034 As
// built, items cleared in a later fiscal year): the system's form, the
// key's length, a line number above 0, a key once per system, one item
// per line of an entry, of a run of the tenant.
func TestOpeningItemRules(t *testing.T) {
	need(t)
	f, other := newFixture(t), newFixture(t)
	run, otherRun := id(), id()
	for _, s := range []struct {
		fx  fixture
		run string
	}{{f, run.String()}, {other, otherRun.String()}} {
		mustApp(t, s.fx.tenant, func(tx pgx.Tx) error {
			connector := id()
			if err := exec(tx, `INSERT INTO integration.connector (tenant_id, id, kind, name, status, created_by, updated_by)
				VALUES ($1, $2, 'sap_file', 'SAP', 'active', $3, $3)`, s.fx.tenant, connector, s.fx.actor); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO integration.migration_run (tenant_id, id, connector_id, scope, mode, is_rehearsal, status, created_by)
				VALUES ($1, $2, $3, '{"fiscal_year": 2025}', 'balances', false, 'loaded', $4)`, s.fx.tenant, s.run, connector, s.fx.actor)
		})
	}
	// Each case's items are of an entry of its own, their keys under its
	// own company code: what a case records is committed.
	var entry uuid.UUID
	var company string
	insert := func(system, key string, line int, of any) func() stmt {
		return func() stmt {
			if key != "" {
				key = company + key
			}
			return step(`INSERT INTO integration.opening_item (tenant_id, system, external_key, journal_entry_id, fiscal_year, line_no, migration_run_id,
				created_by) VALUES ($1, $2, $3, $4, 2025, $5, $6, $7)`, f.tenant, system, key, entry, line, of, f.actor)
		}
	}
	for i, c := range []struct {
		name  string
		steps []func() stmt
		want  string
	}{
		{"an item", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, run)}, ""},
		{"a system of another form", []func() stmt{insert("sap prd", "/2025/0100000002/000002", 3, run)}, "opening_item_system_check"},
		{"an empty key", []func() stmt{insert("SAP_PRD", "", 3, run)}, "opening_item_external_key_check"},
		{"a key too long", []func() stmt{insert("SAP_PRD", strings.Repeat("k", 101), 3, run)}, "opening_item_external_key_check"},
		{"line 0", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 0, run)}, "opening_item_line_no_check"},
		{"a key twice", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, run), insert("SAP_PRD", "/2025/0100000002/000002", 4, run)},
			"opening_item_pkey"},
		{"a key of another system", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, run), insert("SAP_QAS", "/2025/0100000002/000002", 4, run)}, ""},
		{"a line twice", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, run), insert("SAP_PRD", "/2025/0100000003/000002", 3, run)},
			"opening_item_tenant_id_fiscal_year_journal_entry_id_line_no_key"},
		{"a run that does not exist", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, id())}, "opening_item_tenant_id_migration_run_id_fkey"},
		{"another tenant's run", []func() stmt{insert("SAP_PRD", "/2025/0100000002/000002", 3, otherRun)}, "opening_item_tenant_id_migration_run_id_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry, company = id(), fmt.Sprintf("%04d", i)
			steps := make([]stmt, len(c.steps))
			for j, s := range c.steps {
				steps[j] = s()
			}
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, steps...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("insert: got %v, want it recorded", err)
				}
				return
			}
			failsWith(t, err, c.want)
		})
	}
}

// TestOpeningItemPrivileges: the application records an opening item and
// reads it, never changes or deletes one, and sees its own tenant's
// alone.
func TestOpeningItemPrivileges(t *testing.T) {
	need(t)
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_table_privilege('erp_app', 'integration.opening_item', 'INSERT')`, true},
		{`has_table_privilege('erp_app', 'integration.opening_item', 'SELECT')`, true},
		{`has_table_privilege('erp_app', 'integration.opening_item', 'UPDATE')`, false},
		{`has_table_privilege('erp_app', 'integration.opening_item', 'DELETE')`, false},
		{`(SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'integration.opening_item'::regclass)`, true},
	} {
		t.Run(c.check, func(t *testing.T) {
			var got bool
			if err := testDB.QueryRow(context.Background(), `SELECT `+c.check).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestOpeningItemDown: 000499's down refuses, rather than drops, the
// opening items the previous version cannot hold (06 §6.4).
func TestOpeningItemDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000499_opening_item").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
		step(`INSERT INTO integration.opening_item (tenant_id, system, external_key, journal_entry_id, fiscal_year, line_no, migration_run_id, created_by)
			VALUES ($1, 'SAP_PRD', 'k', gen_random_uuid(), 2025, 1, gen_random_uuid(), $1)`, tenant),
		step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "opening items exist")
}
