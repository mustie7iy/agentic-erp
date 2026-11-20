package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The updates of an entering (T65d): a processed document entered as a
// supplier invoice, or held with the problem of its entering.
const (
	enteredSet = "status = 'entered', entry_at = now(), entry_fetch_operation_id = gen_random_uuid(), supplier_invoice_id = gen_random_uuid(), " +
		"supplier_id = gen_random_uuid()"
	heldSet = "status = 'held', problem_code = 'payables.capture_needs_order', problem_detail = 'No order.', entry_at = now(), " +
		"entry_fetch_operation_id = gen_random_uuid()"
)

// TestEInvoiceEntering (T65d, migrations 000421, 000422; ADR-0061 §6, §7):
// a processed document moves from received to entered, naming its invoice
// and supplier, or to held, naming its problem; when and by which fetch
// together, never before its processing; entered and held are final and
// what the entering decided never changes; an unprocessed document is
// neither entered nor held.
func TestEInvoiceEntering(t *testing.T) {
	need(t)
	const row = `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
		created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k', gen_random_uuid(), $1, $1)`
	for _, c := range []struct {
		name string
		set  []string
		want string // empty: allowed
	}{
		{"entered", []string{processedSet, enteredSet}, ""},
		{"held", []string{processedSet, heldSet}, ""},
		{"entered unprocessed", []string{enteredSet}, "cannot change from received to entered"},
		{"held unprocessed", []string{heldSet}, "cannot change from received to held"},
		{"entered, then held", []string{processedSet, enteredSet, "status = 'held', supplier_invoice_id = NULL, supplier_id = NULL, " +
			"problem_code = 'c', problem_detail = 'd'"}, "cannot change from entered to held"},
		{"held, then entered", []string{processedSet, heldSet, "status = 'entered', problem_code = NULL, problem_detail = NULL, " +
			"supplier_invoice_id = gen_random_uuid(), supplier_id = gen_random_uuid()"}, "cannot change from held to entered"},
		{"held, then received again", []string{processedSet, heldSet, "status = 'received', problem_code = NULL, problem_detail = NULL, " +
			"entry_at = NULL, entry_fetch_operation_id = NULL"}, "cannot change from held to received"},
		{"entered, its invoice changed", []string{processedSet, enteredSet, "supplier_invoice_id = gen_random_uuid()"},
			"keeps what its processing read and decided"},
		{"held, its problem changed", []string{processedSet, heldSet, "problem_detail = 'other'"}, "keeps what its processing read and decided"},
		{"entered without its invoice", []string{processedSet, "status = 'entered', entry_at = now(), entry_fetch_operation_id = gen_random_uuid(), " +
			"supplier_id = gen_random_uuid()"}, "einvoice_entered"},
		{"entered without its supplier", []string{processedSet, "status = 'entered', entry_at = now(), entry_fetch_operation_id = gen_random_uuid(), " +
			"supplier_invoice_id = gen_random_uuid()"}, "einvoice_entered"},
		{"held naming an invoice", []string{processedSet, heldSet + ", supplier_invoice_id = gen_random_uuid()"}, "einvoice_entered"},
		{"entered with a problem", []string{processedSet, enteredSet + ", problem_code = 'c', problem_detail = 'd'"}, "einvoice_problem"},
		{"held without its problem", []string{processedSet, "status = 'held', entry_at = now(), entry_fetch_operation_id = gen_random_uuid()"},
			"einvoice_problem"},
		{"entered without when", []string{processedSet, "status = 'entered', entry_fetch_operation_id = gen_random_uuid(), " +
			"supplier_invoice_id = gen_random_uuid(), supplier_id = gen_random_uuid()"}, "einvoice_entry"},
		{"held without its fetch", []string{processedSet, "status = 'held', problem_code = 'c', problem_detail = 'd', entry_at = now()"}, "einvoice_entry"},
		{"entered before its processing", []string{processedSet, "status = 'entered', entry_at = now() - interval '1 hour', " +
			"entry_fetch_operation_id = gen_random_uuid(), supplier_invoice_id = gen_random_uuid(), supplier_id = gen_random_uuid()"}, "einvoice_entry"},
		{"an entry on a received document", []string{processedSet, "entry_at = now(), entry_fetch_operation_id = gen_random_uuid()"}, "einvoice_entry"},
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
	t.Run("the checks validated by 000422", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT bool_and(convalidated) FROM pg_constraint
			WHERE (conrelid = 'einvoicing.einvoice'::regclass AND conname IN ('einvoice_status_check', 'einvoice_entry', 'einvoice_entered', 'einvoice_problem'))
			   OR (conrelid = 'payables.supplier_invoice'::regclass AND conname IN ('supplier_invoice_source_check', 'supplier_invoice_document_hash'))`).
			Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want every one validated", valid, err)
		}
	})
}

// TestEInvoiceEnteringTrigger: the trigger itself holds the entering's
// transitions and what it decided, as the owner, whom the column grants
// do not bind, would change them.
func TestEInvoiceEnteringTrigger(t *testing.T) {
	need(t)
	for name, c := range map[string]struct{ set, want string }{
		"an unprocessed document held": {heldSet, "cannot change from received to held"},
		"an entered document held": {processedSet + ", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET " + enteredSet +
			", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET status = 'held'", "cannot change from entered to held"},
		"an entered document's invoice changed": {processedSet + ", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET " + enteredSet +
			", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET supplier_invoice_id = gen_random_uuid()", "keeps what its processing read and decided"},
		"a held document's fetch changed": {processedSet + ", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET " + heldSet +
			", version = version + 1" + ownRow + "; UPDATE einvoicing.einvoice SET entry_fetch_operation_id = gen_random_uuid()", "keeps what its processing read and decided"},
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
					DROP CONSTRAINT einvoice_entry, DROP CONSTRAINT einvoice_problem`),
				step(`INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref, fetch_operation_id,
					created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'received', sha256('x'::bytea), 1, 'k',
					gen_random_uuid(), $1, $1)`, tenant),
				step(`UPDATE einvoicing.einvoice SET `+c.set+`, version = version + 1`+ownRow))
			failsWith(t, err, c.want)
		})
	}
}

// TestSupplierInvoiceFromEInvoice (T65d, migration 000421): an invoice of
// source einvoice keeps its document's hash, as a captured one does; no
// other source has one.
func TestSupplierInvoiceFromEInvoice(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name, source, sha, want string
	}{
		{"an e-invoice's with its hash", "einvoice", "sha256('x'::bytea)", ""},
		{"an e-invoice's without one", "einvoice", "NULL", "supplier_invoice_document_hash"},
		{"a manual one with a hash", "manual", "sha256('x'::bytea)", "supplier_invoice_document_hash"},
		{"a captured one without one", "capture", "NULL", "supplier_invoice_document_hash"},
		{"another source", "fax", "NULL", "supplier_invoice_source_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			err = all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(`INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
					currency, gross_amount, tax_amount, status, source, capture_sha256, created_by, updated_by)
					VALUES ($1, gen_random_uuid(), 'I1', $1, $1, 'R1', current_date, 'AED', 100, 0, 'parked', '`+c.source+`', `+c.sha+`, $1, $1)`, tenant))
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

// TestEInvoiceEnteringDown: the down migration refuses, rather than drops,
// entered or held e-invoices and invoices entered from them (06 §6.4),
// reading every tenant's rows.
func TestEInvoiceEnteringDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000421_einvoice_entering").Down
	for name, seed := range map[string]string{
		"a held e-invoice": `INSERT INTO einvoicing.einvoice (tenant_id, id, connector_id, message_id, status, sha256, size, content_ref,
			fetch_operation_id, processed_at, processed_fetch_operation_id, validated_against, problem_code, problem_detail, entry_at,
			entry_fetch_operation_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'M', 'held', sha256('x'::bytea), 1,
			'k', gen_random_uuid(), now(), gen_random_uuid(), 'r', 'c', 'd', now(), gen_random_uuid(), $1, $1)`,
		"an invoice entered from one": `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id,
			supplier_reference, invoice_date, currency, gross_amount, tax_amount, status, source, capture_sha256, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'I1', $1, $1, 'R1', current_date, 'AED', 100, 0, 'parked', 'einvoice', sha256('x'::bytea), $1, $1)`,
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
			failsWith(t, exec(tx, down), "entered or held e-invoices exist")
		})
	}
}
