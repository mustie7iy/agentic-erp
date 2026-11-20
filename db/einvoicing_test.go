package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEInvoicingPermissions (T65a, migration 000231; ADR-0061 §11): the
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
	if got, want := strings.Join(codes, " "), "einvoicing.einvoice.manage einvoicing.einvoice.read einvoicing.outgoing.read einvoicing.outgoing.render"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestEInvoiceRules (T65a, migration 000231; ADR-0061 §4, §7): an e-invoice
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
		}, "keeps what was recorded, its acknowledgement"},
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
		"a refusal undone": {refusedSet + ", version = version + 1; UPDATE einvoicing.einvoice SET status = 'received', problem_code = NULL, " +
			"problem_detail = NULL", "cannot change from refused to received"},
		"what processing read changed": {processedSet + ", version = version + 1; UPDATE einvoicing.einvoice SET document_number = 'other'",
			"keeps what its processing read and decided"},
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

// TestEInvoiceProviderKind (migrations 000231, 000232): an integration
// connector may be of the kind einvoice_provider (ADR-0061 §3).
func TestEInvoiceProviderKind(t *testing.T) {
	need(t)
	tenant, actor := id(), id()
	mustApp(t, tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, $2, 'einvoice_provider', 'Provider', '{}', 'active', $3, $3)`, tenant, id(), actor)
	})
}

// TestEInvoiceChange (T65b, migrations 000233, 000234; ADR-0061 §4, §8): a change
// of an e-invoice's bytes is recorded beside its first bytes, its hash,
// time and fetch set together, never to the bytes kept, never before the
// document was received, and never undone; the worker role reads when a
// document was received and acknowledged, for the gauge, and nothing else.
func TestEInvoiceChange(t *testing.T) {
	need(t)
	const row = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
		created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k', gen_random_uuid(), $1, $1)`
	update := func(set string) stmt {
		return step(`UPDATE einvoicing.einvoice SET ` + set + `, version = version + 1`)
	}
	const changed = "changed_hash = sha256('y'::bytea), changed_at = now(), changed_fetch_operation_id = gen_random_uuid()"
	for _, c := range []struct {
		name string
		set  []string
		want string // empty: allowed
	}{
		{"a change recorded, then another", []string{changed, "changed_hash = sha256('z'::bytea), changed_at = now()"}, ""},
		{"a change without its fetch", []string{"changed_hash = sha256('y'::bytea), changed_at = now()"}, "einvoice_changed"},
		{"a change to the bytes kept", []string{"changed_hash = sha256, changed_at = now(), changed_fetch_operation_id = gen_random_uuid()"}, "einvoice_changed"},
		{"a change before the document was received", []string{
			"changed_hash = sha256('y'::bytea), changed_at = now() - interval '1 hour', changed_fetch_operation_id = gen_random_uuid()"}, "einvoice_changed"},
		{"a change of a malformed hash", []string{"changed_hash = 'y'::bytea, changed_at = now(), changed_fetch_operation_id = gen_random_uuid()"},
			"einvoice_changed_hash"},
		{"a change undone", []string{changed, "changed_hash = NULL, changed_at = NULL, changed_fetch_operation_id = NULL"},
			"keeps what was recorded, its acknowledgement and its change of bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant := id()
			steps := []stmt{step(row, tenant)}
			for _, set := range c.set {
				steps = append(steps, update(set))
			}
			err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, steps...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("the checks validated by 000234", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT bool_and(convalidated) FROM pg_constraint
			WHERE conrelid = 'einvoicing.einvoice'::regclass AND conname IN ('einvoice_changed', 'einvoice_changed_hash')`).Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want both validated", valid, err)
		}
	})
	t.Run("the worker role", func(t *testing.T) {
		if err := inRole(t, "erp_worker", func(tx pgx.Tx) error {
			return exec(tx, `SELECT count(*) FROM einvoicing.einvoice WHERE acknowledged_at IS NULL AND received_at < now()`)
		}); err != nil {
			t.Errorf("the gauge's read: got %v, want it allowed", err)
		}
		failsWith(t, inRole(t, "erp_worker", func(tx pgx.Tx) error { return exec(tx, `SELECT sha256 FROM einvoicing.einvoice`) }), "permission denied")
	})
}

// The updates of a processing (T65c): left received with what it read, or
// refused with its problem.
const (
	processedSet = "processed_at = now(), processed_fetch_operation_id = gen_random_uuid(), validated_against = 'PINT AE Billing 1.0.4', " +
		"document_number = 'GOI-1', document_kind = 'invoice', issue_date = '2026-03-10', currency = 'AED', gross_amount = 105000, " +
		"specification = 'urn:peppol:pint:billing-1@ae-1'"
	refusedSet = "status = 'refused', problem_code = 'einvoicing.unreadable', problem_detail = 'Not XML.', processed_at = now(), " +
		"processed_fetch_operation_id = gen_random_uuid(), validated_against = 'PINT AE Billing 1.0.4'"
)

// TestEInvoiceProcessing (T65c, migrations 000416, 000417; ADR-0061 §5, §7):
// a document is recorded unprocessed; processing leaves it received with
// what it read or refuses it, its problem's code and detail set exactly
// then; processed when, by which fetch and against which release
// together, never before receipt; what was read set only when processed,
// a currency with its amount; refused is final and what processing read
// and decided never changes.
func TestEInvoiceProcessing(t *testing.T) {
	need(t)
	const row = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
		created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k', gen_random_uuid(), $1, $1)`
	for _, c := range []struct {
		name string
		set  []string
		want string // empty: allowed
	}{
		{"processed and left received", []string{processedSet}, ""},
		{"refused", []string{refusedSet}, ""},
		{"refused, then processed again", []string{refusedSet, "processed_at = now() + interval '1 hour'"}, "keeps what its processing read and decided"},
		{"refused, then received again", []string{refusedSet, "status = 'received', problem_code = NULL, problem_detail = NULL"},
			"cannot change from refused to received"},
		{"refused without its problem", []string{"status = 'refused', processed_at = now(), validated_against = 'r'"}, "einvoice_problem"},
		{"a problem without its detail", []string{"status = 'refused', problem_code = 'c', processed_at = now(), validated_against = 'r'"},
			"einvoice_problem"},
		{"received with a problem", []string{"problem_code = 'c', problem_detail = 'd', processed_at = now(), validated_against = 'r'"},
			"einvoice_problem"},
		{"refused unprocessed", []string{"status = 'refused', problem_code = 'c', problem_detail = 'd'"}, "einvoice_processed"},
		{"processed without its release", []string{"processed_at = now()"}, "einvoice_processed"},
		{"processed before it was received", []string{"processed_at = now() - interval '1 hour', validated_against = 'r'"}, "einvoice_processed"},
		{"read unprocessed", []string{"document_number = 'GOI-1'"}, "einvoice_read"},
		{"a currency without its amount", []string{"processed_at = now(), validated_against = 'r', currency = 'AED'"}, "einvoice_read"},
		{"an unknown currency", []string{"processed_at = now(), validated_against = 'r', currency = 'XXX', gross_amount = 1"}, "einvoice_currency_fkey"},
		{"another kind", []string{"processed_at = now(), validated_against = 'r', document_kind = 'debit_note'"}, "einvoice_read_lengths"},
		{"a detail too long", []string{"status = 'refused', problem_code = 'c', problem_detail = repeat('d', 2001), processed_at = now(), " +
			"validated_against = 'r'"}, "einvoice_read_lengths"},
		{"a number too long", []string{"processed_at = now(), validated_against = 'r', document_number = repeat('9', 201)"}, "einvoice_read_lengths"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant := id()
			steps := []stmt{step(row, tenant)}
			for _, set := range c.set {
				steps = append(steps, step(`UPDATE einvoicing.einvoice SET `+set+`, version = version + 1`))
			}
			err := asApp(t, tenant, func(tx pgx.Tx) error { return all(tx, steps...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("recorded processed", func(t *testing.T) {
		tenant := id()
		failsWith(t, asApp(t, tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref,
				fetch_operation_id, processed_at, validated_against, created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M',
				'received', sha256('x'::bytea), 1, 'k', gen_random_uuid(), now(), 'r', $1, $1)`, tenant)
		}), "an e-invoice is recorded unprocessed")
	})
	t.Run("the checks validated by 000417", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT bool_and(convalidated) FROM pg_constraint WHERE conrelid = 'einvoicing.einvoice'::regclass
			AND conname IN ('einvoice_status_check', 'einvoice_read_lengths', 'einvoice_processed', 'einvoice_problem', 'einvoice_read')`).Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want every one validated", valid, err)
		}
	})
}
