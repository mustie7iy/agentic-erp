package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// citingRow inserts a supplier invoice of the kind given, with id $2 (the
// test's tenant is $1), triggers off as a seed: a credit memo e-invoices
// enter, or the invoice it cites.
func citingRow(kind string) string {
	return `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
		currency, gross_amount, tax_amount, status, source, capture_sha256, kind, created_by, updated_by)
		VALUES ($1, $2::uuid, 'D' || $2::uuid::text, $1, $1, 'R' || $2::uuid::text, current_date, 'AED', 100, 0, 'parked', 'einvoice',
		sha256($2::uuid::text::bytea), '` +
		kind + `', $1, $1)`
}

// TestSupplierInvoiceCitations (ADR-0061 As built batch 88, migrations
// 000439, 000440): a credit memo keeps its reason, on what credits only,
// its words not without its code, within their lengths; its citations are
// numbered 1 to 100, each number once, not blank and at most 200
// characters, of an invoice of the tenant and citing one; they are never
// changed or deleted, and other tenants' are not seen.
func TestSupplierInvoiceCitations(t *testing.T) {
	need(t)
	const cite = `INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number, invoice_date, cited_invoice_id)
		VALUES ($1, $2, `
	for _, c := range []struct {
		name string
		kind string
		then string // run as the app after the memo $2 and an invoice $3 exist
		want string // empty: allowed
	}{
		{"a memo's reason", "credit_memo", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = 'VD', credit_reason_text = 'Volume' WHERE id = $2 AND tenant_id = $1`, ""},
		{"a code without words", "credit_memo", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = 'VD' WHERE id = $2 AND tenant_id = $1`, ""},
		{"words without a code", "credit_memo", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_text = 'Volume' WHERE id = $2 AND tenant_id = $1`,
			"supplier_invoice_credit_reason"},
		{"an invoice's reason", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = 'VD' WHERE id = $2 AND tenant_id = $1`, "supplier_invoice_credit_reason"},
		{"a code too long", "credit_memo", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = repeat('V', 101) WHERE id = $2 AND tenant_id = $1`,
			"supplier_invoice_credit_reason"},
		{"words too long", "credit_memo", `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = 'VD', credit_reason_text = repeat('w', 1001) WHERE id = $2 AND tenant_id = $1`,
			"supplier_invoice_credit_reason"},
		// 000441, 000442: the tax in the company's currency an e-invoice states.
		{"a stated local tax", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, stated_local_tax = 18363, stated_local_currency = 'AED'
			WHERE id = $2 AND tenant_id = $1`, ""},
		{"a local tax without its currency", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, stated_local_tax = 1
			WHERE id = $2 AND tenant_id = $1`, "supplier_invoice_stated_local_tax"},
		{"a currency without its tax", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, stated_local_currency = 'AED'
			WHERE id = $2 AND tenant_id = $1`, "supplier_invoice_stated_local_tax"},
		{"a negative local tax", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, stated_local_tax = -1, stated_local_currency = 'AED'
			WHERE id = $2 AND tenant_id = $1`, "supplier_invoice_stated_local_tax"},
		{"an unknown currency", "invoice", `UPDATE payables.supplier_invoice SET version = version + 1, stated_local_tax = 1, stated_local_currency = 'XXX'
			WHERE id = $2 AND tenant_id = $1`, "supplier_invoice_stated_local_currency_fkey"},
		{"citations", "credit_memo", cite + `1, 'GOI-1', '2026-03-01', $3), ($1, $2, 2, 'GOI-2', NULL, NULL)`, ""},
		{"position 0", "credit_memo", cite + `0, 'GOI-1', NULL, NULL)`, "supplier_invoice_citation_position_check"},
		{"position 101", "credit_memo", cite + `101, 'GOI-1', NULL, NULL)`, "supplier_invoice_citation_position_check"},
		{"a position twice", "credit_memo", cite + `1, 'GOI-1', NULL, NULL), ($1, $2, 1, 'GOI-2', NULL, NULL)`, "supplier_invoice_citation_pkey"},
		{"a number twice", "credit_memo", cite + `1, 'GOI-1', NULL, NULL), ($1, $2, 2, 'GOI-1', NULL, NULL)`, "supplier_invoice_citation_once"},
		{"a blank number", "credit_memo", cite + `1, ' ', NULL, NULL)`, "supplier_invoice_citation_invoice_number_check"},
		{"a number too long", "credit_memo", cite + `1, repeat('9', 201), NULL, NULL)`, "supplier_invoice_citation_invoice_number_check"},
		{"of no invoice", "credit_memo", `INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number)
			VALUES ($1, gen_random_uuid(), 1, 'GOI-1')`, "supplier_invoice_citation_invoice_fkey"},
		{"citing no invoice", "credit_memo", cite + `1, 'GOI-1', NULL, gen_random_uuid())`, "supplier_invoice_citation_cited_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, memo, invoice := id(), id(), id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(citingRow(c.kind), tenant, memo), step(citingRow("invoice"), tenant, invoice), step(`SET LOCAL session_replication_role = origin`),
				step(`SET LOCAL ROLE erp_app`)); err != nil {
				t.Fatal(err)
			}
			args := []any{tenant, memo, invoice}
			for !strings.Contains(c.then, fmt.Sprintf("$%d", len(args))) {
				args = args[:len(args)-1] // the parameters the statement uses
			}
			_, err = tx.Exec(ctx, c.then, args...)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("the checks validated by 000440 and 000442", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT bool_and(convalidated) FROM pg_constraint WHERE conrelid = 'payables.supplier_invoice'::regclass
			AND conname IN ('supplier_invoice_credit_reason', 'supplier_invoice_stated_local_tax')`).Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want both validated", valid, err)
		}
	})
}

// TestSupplierInvoiceCitationsImmutable: the application may neither
// change nor delete a citation (no grant), and the trigger itself keeps
// one as written, as the owner, whom the grants do not bind, would change
// or delete it; and another tenant's citations are not seen.
func TestSupplierInvoiceCitationsImmutable(t *testing.T) {
	need(t)
	for name, c := range map[string]struct{ role, change, want string }{
		"changed by the app":   {"erp_app", `UPDATE payables.supplier_invoice_citation SET invoice_number = 'GOI-2' WHERE tenant_id = $1`, "permission denied"},
		"deleted by the app":   {"erp_app", `DELETE FROM payables.supplier_invoice_citation WHERE tenant_id = $1`, "permission denied"},
		"changed by the owner": {"erp_owner", `UPDATE payables.supplier_invoice_citation SET invoice_number = 'GOI-2' WHERE tenant_id = $1`, "is insert-only"},
		"deleted by the owner": {"erp_owner", `DELETE FROM payables.supplier_invoice_citation WHERE tenant_id = $1`, "is insert-only"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, memo := id(), id()
			err = all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(citingRow("credit_memo"), tenant, memo), step(`SET LOCAL session_replication_role = origin`),
				step(`INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number) VALUES ($1, $2, 1, 'GOI-1')`,
					tenant, memo),
				step(`SET LOCAL ROLE `+c.role), step(c.change, tenant))
			failsWith(t, err, c.want)
		})
	}
	t.Run("another tenant's", func(t *testing.T) {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		tenant, memo := id(), id()
		if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
			step(citingRow("credit_memo"), tenant, memo), step(`SET LOCAL session_replication_role = origin`),
			step(`INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number) VALUES ($1, $2, 1, 'GOI-1')`,
				tenant, memo),
			step(`SELECT set_config('app.tenant_id', $1, true)`, id().String()), step(`SET LOCAL ROLE erp_app`)); err != nil {
			t.Fatal(err)
		}
		var seen int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payables.supplier_invoice_citation WHERE supplier_invoice_id = $1`, memo).Scan(&seen); err != nil || seen != 0 {
			t.Errorf("got %d (%v), want none of another tenant's", seen, err)
		}
	})
}

// TestSupplierInvoiceCitationsDown: the down migration of 000439 refuses,
// rather than drops, credit memos citing invoices or keeping their reason
// (06 §6.4), reading every tenant's rows.
func TestSupplierInvoiceCitationsDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000439_supplier_invoice_citations").Down
	for name, seed := range map[string]string{
		"a citation": `INSERT INTO payables.supplier_invoice_citation (tenant_id, supplier_invoice_id, position, invoice_number) VALUES ($1, $2, 1, 'GOI-1')`,
		"a reason":   `UPDATE payables.supplier_invoice SET version = version + 1, credit_reason_code = 'VD' WHERE id = $2 AND tenant_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, memo := id(), id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(citingRow("credit_memo"), tenant, memo), step(seed, tenant, memo), step(`SET LOCAL session_replication_role = origin`),
				step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "credit memos citing invoices or keeping their reason exist")
		})
	}
}

// TestStatedLocalTaxDown: the down migration of 000441 refuses, rather
// than drops, invoices keeping their stated local tax (06 §6.4), reading
// every tenant's rows.
func TestStatedLocalTaxDown(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, invoice := id(), id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
		step(citingRow("invoice"), tenant, invoice),
		step(`UPDATE payables.supplier_invoice SET stated_local_tax = 1, stated_local_currency = 'AED' WHERE id = $2 AND tenant_id = $1`, tenant, invoice),
		step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, migrationNamed(t, "000441_supplier_invoice_stated_local_tax").Down), "supplier invoices keeping their stated local tax exist")
}
