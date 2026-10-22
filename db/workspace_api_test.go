package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestAuditClientAddresses: an audit event records where its request came
// from, and stays insert-only but for the erasure of its tenant blanking
// that address, and nothing else (migration 000151, ADR-0011 as amended by
// T73c): not another tenant's erasure, not another column, not a new
// address; a delete only during the erasure, as before.
// platform.erase_tenant blanks the addresses and keeps the events.
func TestAuditClientAddresses(t *testing.T) {
	need(t)
	f := newFixture(t)
	insert := func(tx pgx.Tx, seq int, addr any) error {
		return exec(tx, `INSERT INTO audit.audit_event (tenant_id, seq, actor_id, actor_kind, action, object_type, outcome, hash, client_address)
			VALUES ($1, $2::bigint, $3, 'human', 'test.thing.do', 'test.thing', 'succeeded', sha256($2::bigint::text::bytea), $4)`, f.tenant, seq, f.actor, addr)
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return insert(tx, 1, "203.0.113.24") })
	mustApp(t, f.tenant, func(tx pgx.Tx) error { return insert(tx, 2, nil) })
	t.Run("not an address", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return insert(tx, 3, "somewhere") }), "invalid input syntax for type inet")
	})
	// As the owner, the only role that may change anything, with the
	// erasure's setting for a tenant.
	asOwner := func(erasing uuid.UUID, sql string) error {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.erasing', $2, true)`, f.tenant.String(), erasing.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql+` WHERE tenant_id = $1`, f.tenant); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	for name, c := range map[string]struct {
		erasing uuid.UUID
		sql     string
	}{
		"no erasure":                   {uuid.Nil, `UPDATE audit.audit_event SET client_address = NULL`},
		"another tenant's erasure":     {id(), `UPDATE audit.audit_event SET client_address = NULL`},
		"another column":               {f.tenant, `UPDATE audit.audit_event SET client_address = NULL, outcome = 'failed'`},
		"another address":              {f.tenant, `UPDATE audit.audit_event SET client_address = '198.51.100.1'`},
		"a delete without the erasure": {uuid.Nil, `DELETE FROM audit.audit_event`},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asOwner(c.erasing, c.sql), "insert-only")
		})
	}
	// The application role has no grant to change events at all.
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE audit.audit_event SET client_address = NULL WHERE tenant_id = $1`, f.tenant)
	}), "permission denied")
	var counts map[string]any
	if err := testDB.QueryRow(context.Background(), `SELECT platform.erase_tenant($1)`, f.tenant).Scan(&counts); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if got := counts["audit.audit_event.client_address"]; got != float64(1) {
		t.Errorf("addresses erased: got %v, want 1 (counts %v)", got, counts)
	}
	var events, addresses int
	if err := testDB.QueryRow(context.Background(), `SELECT count(*), count(client_address) FROM audit.audit_event WHERE tenant_id = $1`, f.tenant).
		Scan(&events, &addresses); err != nil {
		t.Fatal(err)
	}
	if events != 2 || addresses != 0 {
		t.Errorf("after the erasure: got %d events with %d addresses, want 2 events, no address", events, addresses)
	}
}

// TestSelfReadIsEveryonesOwn: identity.self.read is seeded, not an approval
// permission, and granted to no role by the migration (every principal
// holds it, authz.Own).
func TestSelfReadIsEveryonesOwn(t *testing.T) {
	need(t)
	var approval bool
	var granted int
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval, (SELECT count(*) FROM identity.role_permission WHERE permission_code = code)
		FROM ref.permission WHERE code = 'identity.self.read'`).Scan(&approval, &granted); err != nil {
		t.Fatalf("identity.self.read: %v", err)
	}
	if approval || granted != 0 {
		t.Errorf("identity.self.read: got approval %v, granted to %d roles, want neither", approval, granted)
	}
}
