package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEInvoicingPermissions (T65a, migration 000230; ADR-0061 §11): the
// e-invoice read and manage permissions are seeded and are no approval
// permissions.
func TestEInvoicingPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'einvoicing.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "einvoicing.einvoice.manage einvoicing.einvoice.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestEInvoiceRules (T65a, migration 000230; ADR-0061 §4, §7): an e-invoice
// is recorded received, once per connector and message id; its status does
// not move yet (T65c-T65f widen the trigger); what was recorded is fixed,
// its acknowledgement set once and never before it was received; it is
// never deleted.
func TestEInvoiceRules(t *testing.T) {
	need(t)
	const row = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, receiver, legal_entity_id, status, sha256, size,
		content_ref, fetch_operation_id, created_by, updated_by) VALUES ($1, $2, $3, $4, '0235:100000000000003', gen_random_uuid(), $5,
		sha256('x'::bytea), 120, 'tenants/t/documents/ab', gen_random_uuid(), $6, $6)`
	type scene struct{ tenant, actor, connector, doc uuid.UUID }
	recorded := func(s scene) stmt { return step(row, s.tenant, s.doc, s.connector, "MSG-1", "received", s.actor) }
	update := func(set string) stmt {
		return step(`UPDATE einvoicing.einvoice SET ` + set + `, version = version + 1`)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a document recorded and acknowledged", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), update("acknowledged_at = now()"))
		}, ""},
		{"a document recorded in another status", func(tx pgx.Tx, s scene) error {
			return exec(tx, row, s.tenant, s.doc, s.connector, "MSG-1", "entered", s.actor)
		}, "an e-invoice is recorded received, not entered"},
		{"a message recorded twice", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), step(row, s.tenant, id(), s.connector, "MSG-1", "received", s.actor))
		}, "einvoice_message_once"},
		{"the same message id of another connector", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), step(row, s.tenant, id(), id(), "MSG-1", "received", s.actor))
		}, ""},
		{"a message id empty", func(tx pgx.Tx, s scene) error {
			return exec(tx, row, s.tenant, s.doc, s.connector, "", "received", s.actor)
		}, "einvoice_message_id_check"},
		{"a document's bytes changed", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), update("sha256 = sha256('y'::bytea)"))
		}, "permission denied"},
		{"a document moved to another company", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), update("legal_entity_id = gen_random_uuid()"))
		}, "permission denied"},
		{"an acknowledgement moved", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), update("acknowledged_at = now()"), update("acknowledged_at = now() + interval '1 hour'"))
		}, "keeps what was recorded and its acknowledgement"},
		{"an acknowledgement before the document was received", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), update("acknowledged_at = now() - interval '1 hour'"))
		}, "einvoice_acknowledged"},
		{"a document deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, recorded(s), step(`DELETE FROM einvoicing.einvoice`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), connector: id(), doc: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
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

// TestEInvoiceTrigger: the trigger itself refuses a status change and a
// change of what was recorded, as the owner, whom the column grants do
// not bind, would make them.
func TestEInvoiceTrigger(t *testing.T) {
	need(t)
	for name, c := range map[string]struct{ set, want string }{
		"a status moved":             {"status = 'entered'", "cannot change from received to entered"},
		"a document's bytes changed": {"sha256 = sha256('y'::bytea)", "keeps what was recorded"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
				t.Fatal(err)
			}
			err = all(tx, step(`ALTER TABLE einvoicing.einvoice DROP CONSTRAINT einvoice_status_check`),
				step(`INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
					created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k',
					gen_random_uuid(), $1, $1)`, tenant),
				step(`UPDATE einvoicing.einvoice SET `+c.set+`, version = version + 1`))
			failsWith(t, err, c.want)
		})
	}
}

// TestEInvoiceIsTheTenants: another tenant neither sees an e-invoice nor
// acknowledges it.
func TestEInvoiceIsTheTenants(t *testing.T) {
	need(t)
	a, b, actor := id(), id(), id()
	mustApp(t, a, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref,
			fetch_operation_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1,
			'k', gen_random_uuid(), $2, $2)`, a, actor)
	})
	mustApp(t, b, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM einvoicing.einvoice`).Scan(&n); err != nil || n != 0 {
			t.Errorf("another tenant's e-invoices: got %d (%v), want none", n, err)
		}
		tag, err := tx.Exec(context.Background(), `UPDATE einvoicing.einvoice SET acknowledged_at = now()`)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("another tenant's acknowledgement: got %d rows, want none", tag.RowsAffected())
		}
		return err
	})
}

// TestEInvoiceProviderKind (migrations 000230, 000231): an integration
// connector may be of the kind einvoice_provider (ADR-0061 §3).
func TestEInvoiceProviderKind(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	mustApp(t, tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, $2, 'einvoice_provider', 'Provider', '{}', 'active', $3, $3)`, tenant, id(), actor)
	})
}
