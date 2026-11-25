package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The updates of a person's resolution of a held document (T65f): retried
// (entered, or held again with that attempt's problem), linked (entered)
// or dismissed with a reason, each with a new resolved_at.
const (
	retriedEnteredSet = "status = 'entered', problem_code = NULL, problem_detail = NULL, supplier_invoice_id = gen_random_uuid(), " +
		"supplier_id = gen_random_uuid(), resolution = 'retried', resolved_at = now()"
	retriedHeldSet = "problem_code = 'payables.capture_needs_order', problem_detail = 'Still no order.', resolution = 'retried', resolved_at = now()"
	linkedSet      = "status = 'entered', problem_code = NULL, problem_detail = NULL, supplier_invoice_id = gen_random_uuid(), " +
		"supplier_id = gen_random_uuid(), resolution = 'linked', resolved_at = now()"
	dismissedSet = "status = 'dismissed', resolution = 'dismissed', resolved_at = now(), dismissal_reason = 'Sent twice.'"
)

// TestEInvoiceResolution (T65f, migrations 000469, 000470; ADR-0061 §7): a
// held document moves by a person's resolution, and by nothing else, to
// entered (retried or linked, naming its invoice), to held again (retried,
// with that attempt's problem) or to dismissed (with its reason, keeping
// its problem); what the fetch decided stays; entered and dismissed are
// final; a resolution is a held document's, after the fetch's entry; the
// reason is a text of at most 500 characters, exactly when dismissed.
func TestEInvoiceResolution(t *testing.T) {
	need(t)
	const row = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
		created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k', gen_random_uuid(), $1, $1)`
	held := []string{processedSet, heldSet}
	for _, c := range []struct {
		name string
		set  []string
		want string // empty: allowed
	}{
		{"retried and entered", append(held, retriedEnteredSet), ""},
		{"retried and held again", append(held, retriedHeldSet), ""},
		{"linked", append(held, linkedSet), ""},
		{"dismissed", append(held, dismissedSet), ""},
		{"retried twice", append(held, retriedHeldSet, "resolved_at = now() + interval '1 minute'"), ""},
		{"a resolution erased", append(held, retriedHeldSet, "resolution = NULL, resolved_at = NULL, problem_detail = 'Other.'"),
			"keeps what its processing read and decided"},
		{"a resolution moved back", append(held, retriedHeldSet, "resolved_at = now() - interval '1 minute', problem_detail = 'Other.'"),
			"keeps what its processing read and decided"},
		{"entered without a resolution", append(held, "status = 'entered', problem_code = NULL, problem_detail = NULL, "+
			"supplier_invoice_id = gen_random_uuid(), supplier_id = gen_random_uuid()"), "cannot change from held to entered"},
		{"its problem changed without a resolution", append(held, "problem_detail = 'Other.'"), "keeps what its processing read and decided"},
		{"its fetch changed by a resolution", append(held, retriedHeldSet+", entry_fetch_operation_id = gen_random_uuid()"),
			"keeps what its processing read and decided"},
		{"received again by a resolution", append(held, "status = 'received', problem_code = NULL, problem_detail = NULL, entry_at = NULL, "+
			"entry_fetch_operation_id = NULL, resolution = 'retried', resolved_at = now()"), "cannot change from held to received"},
		{"dismissed, then entered", append(held, dismissedSet, linkedSet+" + interval '1 minute', dismissal_reason = NULL"),
			"cannot change from dismissed to entered"},
		{"linked, its resolution changed", append(held, linkedSet, "resolution = 'retried', resolved_at = now() + interval '1 minute'"),
			"keeps what its processing read and decided"},
		{"a received document resolved", []string{processedSet, "resolution = 'retried', resolved_at = now()"}, "keeps what its processing read and decided"},
		{"dismissed without its reason", append(held, "status = 'dismissed', resolution = 'dismissed', resolved_at = now()"), "einvoice_dismissal_reason"},
		{"dismissed with a blank reason", append(held, "status = 'dismissed', resolution = 'dismissed', resolved_at = now(), dismissal_reason = '  '"),
			"einvoice_dismissal_reason"},
		{"dismissed with a reason too long", append(held, "status = 'dismissed', resolution = 'dismissed', resolved_at = now(), "+
			"dismissal_reason = '"+strings.Repeat("r", 501)+"'"), "einvoice_dismissal_reason"},
		{"a reason not dismissed", append(held, retriedHeldSet+", dismissal_reason = 'Why'"), "einvoice_dismissal_reason"},
		{"dismissed without its problem", append(held, dismissedSet+", problem_code = NULL, problem_detail = NULL"), "einvoice_problem"},
		{"dismissed as retried", append(held, "status = 'dismissed', resolution = 'retried', resolved_at = now(), dismissal_reason = 'Why'"),
			"einvoice_resolved"},
		{"linked, still held", append(held, "resolution = 'linked', resolved_at = now()"), "einvoice_resolved"},
		{"a resolution without when", []string{processedSet, heldSet + ", resolution = 'retried'"}, "einvoice_resolved"},
		{"resolved before the fetch held it", append(held, "resolution = 'retried', resolved_at = now() - interval '1 hour'"), "einvoice_resolved"},
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
	t.Run("an invoice named by one document", func(t *testing.T) {
		const named = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
			processed_at, processed_fetch_operation_id, validated_against, document_kind, entry_at, entry_fetch_operation_id, supplier_invoice_id, supplier_id,
			created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), $3, 'entered', sha256('x'::bytea), 1, 'k', gen_random_uuid(), now(),
			gen_random_uuid(), 'r', 'invoice', now(), gen_random_uuid(), $2, gen_random_uuid(), $1, $1)`
		tenant, invoice := id(), id()
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		err = all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
			step(named, tenant, invoice, "M1"), step(named, tenant, invoice, "M2"))
		failsWith(t, err, "einvoice_invoice_once")
	})
	t.Run("the checks validated by 000470", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT bool_and(convalidated) FROM pg_constraint
			WHERE (conrelid = 'einvoicing.einvoice'::regclass AND conname IN ('einvoice_status_check', 'einvoice_entry', 'einvoice_problem',
				'einvoice_resolved', 'einvoice_dismissal_reason'))
			   OR (conrelid = 'payables.supplier_invoice'::regclass AND conname = 'supplier_invoice_document_hash')`).Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want every one validated", valid, err)
		}
	})
}

// TestEInvoiceResolutionTrigger: the trigger itself holds a held
// document's resolution and keeps an entered or dismissed one, as the
// owner, whom the column grants do not bind, would change them.
func TestEInvoiceResolutionTrigger(t *testing.T) {
	need(t)
	held := processedSet + ", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET " + heldSet + ", version = version + 1" + ownRow +
		"; UPDATE einvoicing.einvoice SET "
	for name, c := range map[string]struct{ set, want string }{
		"held to entered without a resolution":   {held + "status = 'entered'", "cannot change from held to entered"},
		"held to dismissed without a resolution": {held + "status = 'dismissed'", "cannot change from held to dismissed"},
		"a held document's problem changed":      {held + "problem_code = 'other'", "keeps what its processing read and decided"},
		"a dismissed document's reason changed": {held + dismissedSet + ", version = version + 1" + ownRow +
			"; UPDATE einvoicing.einvoice SET dismissal_reason = 'Other.'", "keeps what its processing read and decided"},
		"a received document resolved": {processedSet + ", version = version + 1" + ownRow +
			"; UPDATE einvoicing.einvoice SET resolution = 'retried', resolved_at = now()", "keeps what its processing read and decided"},
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
			err = all(tx, step(`ALTER TABLE einvoicing.einvoice DROP CONSTRAINT einvoice_status_check, DROP CONSTRAINT einvoice_entered,
					DROP CONSTRAINT einvoice_entry, DROP CONSTRAINT einvoice_problem, DROP CONSTRAINT einvoice_resolved, DROP CONSTRAINT einvoice_dismissal_reason`),
				step(`INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
					created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k',
					gen_random_uuid(), $1, $1)`, tenant),
				step(`UPDATE einvoicing.einvoice SET `+c.set+`, version = version + 1`+ownRow))
			failsWith(t, err, c.want)
		})
	}
}

// TestEInvoiceResolutionDown: the down migration of 000469 refuses, rather
// than drops, e-invoices a person resolved and invoices entered by hand
// that carry a linked e-invoice's document (06 §6.4), reading every
// tenant's rows.
func TestEInvoiceResolutionDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000469_einvoice_resolution").Down
	for name, seed := range map[string]string{
		"a dismissed e-invoice": `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref,
			fetch_operation_id, processed_at, processed_fetch_operation_id, validated_against, problem_code, problem_detail, entry_at,
			entry_fetch_operation_id, document_kind, resolution, resolved_at, dismissal_reason, created_by, updated_by) VALUES ($1, gen_random_uuid(),
			gen_random_uuid(), 'M', 'dismissed', sha256('x'::bytea), 1, 'k', gen_random_uuid(), now(), gen_random_uuid(), 'r', 'c', 'd', now(),
			gen_random_uuid(), 'invoice', 'dismissed', now(), 'Sent twice.', $1, $1)`,
		"an invoice entered by hand with a linked document": `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id,
			supplier_id, supplier_reference, invoice_date, currency, gross_amount, tax_amount, status, source, capture_sha256, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'I1', $1, $1, 'R1', current_date, 'AED', 100, 0, 'parked', 'manual', sha256('x'::bytea), $1, $1)`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(seed, tenant), step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "e-invoices resolved by people exist")
		})
	}
}
