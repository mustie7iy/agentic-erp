package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// outgoingRow inserts an outgoing e-invoice (migration
// 000415_outgoing_einvoices; ADR-0061 As built E2): $1 tenant, $2 id, $3
// customer invoice, $4 kind, $5 type code, $6 status, $7 problems, $8
// sha256, $9 credit reason code, $10 its text, $11 actor.
const outgoingRow = `INSERT INTO einvoicing.outgoing_einvoice (tenant_id, id, legal_entity_id, customer_invoice_id, customer_invoice_number,
	customer_id, kind, type_code, issue_date, currency, net_amount, tax_amount, gross_amount, credit_reason_code, credit_reason_text,
	specification, status, problems, sha256, size, content_ref, created_by, updated_by)
	VALUES ($1, $2, gen_random_uuid(), $3, 'CI100001', gen_random_uuid(), $4, $5, '2026-03-10', 'AED', 1000, 50, 1050, $9, $10,
		'urn:peppol:pint:billing-1@ae-1', $6, $7::jsonb, $8, CASE WHEN $8::bytea IS NULL THEN NULL ELSE 4096 END,
		CASE WHEN $8::bytea IS NULL THEN NULL ELSE 'tenants/t/documents/ab' END, $11, $11)`

// outgoingScene is one test's tenant, actor, e-invoice and customer
// document.
type outgoingScene struct{ tenant, actor, doc, invoice uuid.UUID }

func (s outgoingScene) valid() stmt {
	return step(outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "valid", "[]", []byte("0123456789abcdef0123456789abcdef"), nil, nil, s.actor)
}

func (s outgoingScene) invalid() stmt {
	return step(outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "invalid", `[{"rule": "r", "term": "IBT-048", "message": "m"}]`, nil, nil, nil, s.actor)
}

// TestOutgoingEInvoicePermissions: the two permissions of outgoing
// e-invoices are seeded, neither an approval permission.
func TestOutgoingEInvoicePermissions(t *testing.T) {
	need(t)
	var n int
	if err := testDB.QueryRow(context.Background(), `SELECT count(*) FROM ref.permission
		WHERE code IN ('einvoicing.outgoing.read', 'einvoicing.outgoing.render') AND NOT is_approval`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("got %d, want 2", n)
	}
}

// TestOutgoingEInvoiceRules: what the database refuses of an outgoing
// e-invoice, as the application role: one per customer document; a tax
// invoice 380 and a tax credit note 381; a credit note's reason whole and
// an invoice's none; a valid one with its XML and no problem, an invalid
// one with problems and no XML; an invalid one rendered again, a valid
// one final; the customer document it names fixed; never deleted.
func TestOutgoingEInvoiceRules(t *testing.T) {
	need(t)
	update := func(set string) stmt {
		return step(`UPDATE einvoicing.outgoing_einvoice SET ` + set + `, version = version + 1`)
	}
	sum := []byte("0123456789abcdef0123456789abcdef")
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s outgoingScene) error
		want string // empty: allowed
	}{
		{"a valid tax invoice", func(tx pgx.Tx, s outgoingScene) error { return all(tx, s.valid()) }, ""},
		{"an invalid one made valid", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`status = 'valid', problems = '[]', sha256 = sha256('x'::bytea), size = 10, content_ref = 'k'`))
		}, ""},
		{"an invalid one rendered invalid again", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`problems = '[{"rule": "r2"}]'`))
		}, ""},
		{"a tax credit note with its reason", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "credit_note", "381", "valid", "[]", sum, "CR-01", "Returned", s.actor)
		}, ""},
		{"a customer document rendered twice", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.valid(), step(outgoingRow, s.tenant, id(), s.invoice, "invoice", "380", "valid", "[]", sum, nil, nil, s.actor))
		}, "outgoing_einvoice_once"},
		{"an invoice of type 381", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "381", "valid", "[]", sum, nil, nil, s.actor)
		}, "outgoing_einvoice_type"},
		{"a debit note", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "debit_note", "383", "valid", "[]", sum, nil, nil, s.actor)
		}, "outgoing_einvoice_kind_check"},
		{"an invoice with a credit reason", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "valid", "[]", sum, "CR-01", "Returned", s.actor)
		}, "outgoing_einvoice_reason"},
		{"a reason's code without its text", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "credit_note", "381", "valid", "[]", sum, "CR-01", nil, s.actor)
		}, "outgoing_einvoice_reason"},
		{"a reason's code malformed", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "credit_note", "381", "valid", "[]", sum, "CR 01", "Returned", s.actor)
		}, "outgoing_einvoice_credit_reason_code_check"},
		{"a reason's text over 500 characters", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "credit_note", "381", "valid", "[]", sum, "CR-01", strings.Repeat("x", 501), s.actor)
		}, "outgoing_einvoice_credit_reason_text_check"},
		{"a valid one without its XML", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "valid", "[]", nil, nil, nil, s.actor)
		}, "outgoing_einvoice_content"},
		{"a valid one with problems", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "valid", `[{"rule": "r"}]`, sum, nil, nil, s.actor)
		}, "outgoing_einvoice_content"},
		{"an invalid one without problems", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "invalid", "[]", nil, nil, nil, s.actor)
		}, "outgoing_einvoice_content"},
		{"problems not a list", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "invalid", `{"rule": "r"}`, nil, nil, nil, s.actor)
		}, "outgoing_einvoice_problems_check"},
		{"a source document's id without its number", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`source_document_id = gen_random_uuid()`))
		}, "outgoing_einvoice_source"},
		{"a customer invoice number empty", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, strings.Replace(outgoingRow, "'CI100001'", "''", 1), s.tenant, s.doc, s.invoice, "invoice", "380", "valid", "[]", sum, nil, nil, s.actor)
		}, "outgoing_einvoice_customer_invoice_number_check"},
		{"a source document number over 40 characters", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`source_document_id = gen_random_uuid(), source_document_number = repeat('x', 41)`))
		}, "outgoing_einvoice_source_document_number_check"},
		{"a credited invoice's number empty", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`reference_invoice_number = ''`))
		}, "outgoing_einvoice_reference_invoice_number_check"},
		{"a specification empty", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`specification = ''`))
		}, "outgoing_einvoice_specification_check"},
		{"an object key over 300 characters", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, strings.Replace(outgoingRow, "'tenants/t/documents/ab'", "repeat('k', 301)", 1), s.tenant, s.doc, s.invoice, "invoice", "380",
				"valid", "[]", sum, nil, nil, s.actor)
		}, "outgoing_einvoice_content_ref_check"},
		{"a hash of 31 bytes", func(tx pgx.Tx, s outgoingScene) error {
			return exec(tx, outgoingRow, s.tenant, s.doc, s.invoice, "invoice", "380", "valid", "[]", sum[:31], nil, nil, s.actor)
		}, "outgoing_einvoice_sha256_check"},
		{"a size of zero", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`status = 'valid', problems = '[]', sha256 = sha256('x'::bytea), size = 0, content_ref = 'k'`))
		}, "outgoing_einvoice_size_check"},
		{"an unknown currency", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`currency = 'XXQ'`))
		}, "outgoing_einvoice_currency_fkey"},
		{"a valid one changed", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.valid(), update(`problems = '[{"rule": "r"}]', status = 'invalid', sha256 = NULL, size = NULL, content_ref = NULL`))
		}, "is valid and final"},
		{"an invalid one moved to another customer document", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), update(`customer_invoice_id = gen_random_uuid()`))
		}, "keeps its customer document"},
		{"an e-invoice deleted", func(tx pgx.Tx, s outgoingScene) error {
			return all(tx, s.invalid(), step(`DELETE FROM einvoicing.outgoing_einvoice`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
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

// TestOutgoingEInvoiceTrigger: the trigger holds a valid e-invoice final
// and an invalid one's customer document for the owner too, whom the
// grants do not bind.
func TestOutgoingEInvoiceTrigger(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		valid     bool
		set, want string
	}{
		"a valid one changed":            {true, "rendered_at = now()", "is valid and final"},
		"an invalid one's kind changed":  {false, "kind = 'credit_note', type_code = '381'", "keeps its customer document"},
		"an invalid one's company moved": {false, "legal_entity_id = gen_random_uuid()", "keeps its customer document"},
	} {
		t.Run(name, func(t *testing.T) {
			s := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
			row := s.invalid()
			if c.valid {
				row = s.valid()
			}
			err := asApp(t, s.tenant, asOwner(func(tx pgx.Tx) error {
				return all(tx, row, step(`UPDATE einvoicing.outgoing_einvoice SET `+c.set+`, version = version + 1`))
			}))
			failsWith(t, err, c.want)
		})
	}
}

// TestOutgoingEInvoiceIsTheTenants: another tenant neither sees an
// outgoing e-invoice nor renders it again.
func TestOutgoingEInvoiceIsTheTenants(t *testing.T) {
	need(t)
	a := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
	mustApp(t, a.tenant, func(tx pgx.Tx) error { return all(tx, a.invalid()) })
	mustApp(t, id(), func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM einvoicing.outgoing_einvoice`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("another tenant's outgoing e-invoices: got %d, want none", n)
		}
		tag, err := tx.Exec(context.Background(), `UPDATE einvoicing.outgoing_einvoice SET version = version + 1`)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("another tenant's update: got %d rows, want none", tag.RowsAffected())
		}
		return err
	})
}

// TestOutgoingEInvoicesDownRefused: 000415's down migration refuses to
// drop outgoing e-invoices (06 §6.4: a down migration loses no data).
func TestOutgoingEInvoicesDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := outgoingScene{tenant: id(), actor: id(), doc: id(), invoice: id()}
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.invalid(),
		step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT owner`), step(`SET LOCAL ROLE erp_owner`)); err != nil {
		t.Fatal(err)
	}
	down := migrationNamed(t, "000415_outgoing_einvoices").Down
	// As the owner, whom the forced row-level security binds, the guard
	// fails rather than read no rows (06 §6.4); as the migrator, with no
	// tenant set, it reads every tenant's rows and refuses.
	failsWith(t, exec(tx, down), "row-level security")
	if err := all(tx, step(`ROLLBACK TO SAVEPOINT owner`), step(`RESET ROLE`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "outgoing e-invoices exist")
}
