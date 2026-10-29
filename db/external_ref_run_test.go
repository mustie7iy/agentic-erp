package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// An external reference names the migration run that loaded it (000168,
// review:batch23): a run of the reference's own tenant, or none for one
// recorded before.
func TestExternalRefNamesItsRun(t *testing.T) {
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
				VALUES ($1, $2, $3, '{"fiscal_year": 2026}', 'documents', false, 'loaded', $4)`, s.fx.tenant, s.run, connector, s.fx.actor)
		})
	}
	for _, c := range []struct {
		name string
		run  any
		want string
	}{
		{"its tenant's run", run, ""},
		{"no run, as recorded before", nil, ""},
		{"a run that does not exist", id(), "violates foreign key constraint"},
		{"another tenant's run", otherRun, "violates foreign key constraint"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO integration.external_ref (tenant_id, entity_type, entity_id, system, object, external_key, created_by,
					migration_run_id) VALUES ($1, 'procurement.purchase_order', $2, 'SAP_PRD', 'EKKO', $3, $4, $5)`,
					f.tenant, id(), id().String(), f.actor, c.run)
			})
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
