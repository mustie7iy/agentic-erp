package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReceivablesPermissions (T75a, migration 000265; T75c, migration
// 000272; ADR-0063 §8): the customer invoice permissions and the one to
// apply incoming payments are seeded and are no approval permissions.
func TestReceivablesPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'receivables.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "receivables.invoice.post receivables.invoice.read receivables.invoice.write receivables.payment.apply"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestCustomerInvoiceRules (T75a, migration 000265; ADR-0063 §2-§4): a
// customer invoice is entered as a draft invoice, numbered once per
// company, its amounts not negative and its gross its net and tax; an
// item is a positive net amount with tax only under a tax code; what was
// entered is fixed and never deleted (posting is the one change,
// TestCustomerInvoicePosting).
func TestCustomerInvoiceRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, '2026-03-10', 'EUR', $6, $7, $8, $9, $10, $10)`
	const item = `INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, account_id, net_amount, tax_code, tax_amount)
		VALUES ($1, $2, $3, gen_random_uuid(), $4, $5, $6)`
	type scene struct{ tenant, actor, company, doc uuid.UUID }
	invoice := func(s scene, doc uuid.UUID, number, kind string, net, tax, gross int64, status string) stmt {
		return step(header, s.tenant, doc, number, s.company, kind, net, tax, gross, status, s.actor)
	}
	entered := func(s scene) stmt { return invoice(s, s.doc, "CI1", "invoice", 10000, 1900, 11900, "draft") }
	var none *string
	code := "A1"
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a draft invoice with its items", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 10000, &code, 1900), step(item, s.tenant, s.doc, 2, 500, none, 0))
		}, ""},
		{"entered in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", 10000, 1900, 11900, "paid"))
		}, "customer_invoice_status_check"},
		{"entered as posted, without an entry", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", 10000, 1900, 11900, "posted"))
		}, "customer_invoice_posted"},
		{"of another kind", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "debit_memo", 10000, 1900, 11900, "draft"))
		}, "customer_invoice_kind_check"},
		{"a gross that is not net and tax", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", 10000, 1900, 11901, "draft"))
		}, "customer_invoice_totals"},
		{"a negative net amount", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", -10000, 0, -10000, "draft"))
		}, "customer_invoice_amounts_not_negative"},
		{"a negative tax amount", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", 10000, -1, 9999, "draft"))
		}, "customer_invoice_amounts_not_negative"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), invoice(s, id(), "CI1", "invoice", 1, 0, 1, "draft"))
		}, "customer_invoice_number_once"},
		{"the same number in another company", func(tx pgx.Tx, s scene) error {
			other := s
			other.company = id()
			return all(tx, entered(s), invoice(other, id(), "CI1", "invoice", 1, 0, 1, "draft"))
		}, ""},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(strings.Replace(header, "'EUR'", "'XXA'", 1), s.tenant, s.doc, "CI1", s.company, "invoice", 1, 0, 1, "draft", s.actor))
		}, "customer_invoice_currency_fkey"},
		{"an amount changed", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(`UPDATE receivables.customer_invoice SET gross_amount = 1, net_amount = 1, tax_amount = 0, version = version + 1`))
		}, "permission denied"},
		{"a draft changed without being posted", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(`UPDATE receivables.customer_invoice SET posting_date = '2026-03-11', version = version + 1`))
		}, "only a draft changes, and only by being posted"},
		{"an invoice deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(`DELETE FROM receivables.customer_invoice`))
		}, "permission denied"},
		{"an item of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 0, none, 0))
		}, "customer_invoice_item_net_positive"},
		{"an item's tax negative", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 100, &code, -1))
		}, "customer_invoice_item_tax_not_negative"},
		{"an item's tax without a tax code", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 100, none, 19))
		}, "customer_invoice_item_tax_needs_code"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 100, none, 0), step(item, s.tenant, s.doc, 1, 100, none, 0))
		}, "customer_invoice_item_pkey"},
		{"an item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 0, 100, none, 0))
		}, "customer_invoice_item_item_no_check"},
		{"an item of no invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, step(item, s.tenant, s.doc, 1, 100, none, 0))
		}, "customer_invoice_item_tenant_id_customer_invoice_id_fkey"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 100, none, 0), step(`UPDATE receivables.customer_invoice_item SET net_amount = 200`))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(item, s.tenant, s.doc, 1, 100, none, 0), step(`DELETE FROM receivables.customer_invoice_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), company: id(), doc: id()}
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

// TestCustomerInvoicePosting (T75b, migration 000271; ADR-0063 §5): a
// draft moves to posted once, naming its journal entry, posting date,
// payment term and net due date together; nothing else changes it, a
// posted invoice is not changed again, and a journal entry is one
// invoice's.
func TestCustomerInvoicePosting(t *testing.T) {
	need(t)
	const draft = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, payment_term_code)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 'invoice', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $1, $1, $5)`
	const post = `UPDATE receivables.customer_invoice SET status = 'posted', posting_date = $2, payment_term_code = $3, net_due_date = $4,
		journal_entry_year = $5, journal_entry_id = $6, version = version + 1, updated_by = tenant_id WHERE id = $1`
	type scene struct{ tenant, company, doc, entry uuid.UUID }
	var none *string
	var noYear *int16
	var noEntry *uuid.UUID
	year, term := int16(2026), "NT30"
	posted := func(s scene) stmt { return step(post, s.doc, "2026-03-12", &term, "2026-04-09", &year, &s.entry) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a draft with a payment term, posted", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &term), posted(s))
		}, ""},
		{"a draft without one, posted with the customer's", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), posted(s))
		}, ""},
		{"a payment term of 21 characters", func(tx pgx.Tx, s scene) error {
			long := "123456789012345678901"
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &long))
		}, "customer_invoice_payment_term_code_check"},
		{"posted without its entry", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(post, s.doc, "2026-03-12", &term, "2026-04-09", noYear, noEntry))
		}, "customer_invoice_posted"},
		{"posted without its entry's year", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(post, s.doc, "2026-03-12", &term, "2026-04-09", noYear, &s.entry))
		}, "customer_invoice_posted"},
		{"posted without a net due date", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(post, s.doc, "2026-03-12", &term, none, &year, &s.entry))
		}, "customer_invoice_posted"},
		{"posted without a payment term", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(post, s.doc, "2026-03-12", none, "2026-04-09", &year, &s.entry))
		}, "customer_invoice_posted"},
		{"posted without a posting date", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(post, s.doc, none, &term, "2026-04-09", &year, &s.entry))
		}, "customer_invoice_posted"},
		{"a draft given an entry", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none),
				step(`UPDATE receivables.customer_invoice SET journal_entry_year = 2026, journal_entry_id = $1, net_due_date = '2026-04-09', version = version + 1`, s.entry))
		}, "only a draft changes, and only by being posted"},
		{"posted twice", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), posted(s), posted(s))
		}, "only a draft changes, and only by being posted"},
		{"a posted invoice moved back to draft", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), posted(s),
				step(`UPDATE receivables.customer_invoice SET status = 'draft', journal_entry_year = NULL, journal_entry_id = NULL, net_due_date = NULL, version = version + 1`))
		}, "only a draft changes, and only by being posted"},
		{"posted without moving its version", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(strings.Replace(post, "version = version + 1, ", "", 1),
				s.doc, "2026-03-12", &term, "2026-04-09", &year, &s.entry))
		}, "optimistic lock"},
		{"two invoices posting one entry", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), step(draft, s.tenant, other, "CI2", s.company, none), posted(s),
				step(post, other, "2026-03-12", &term, "2026-04-09", &year, &s.entry))
		}, "customer_invoice_entry_once"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), doc: id(), entry: id()}
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

// TestCustomerCreditMemoRules (T75d, migration 000276; ADR-0063 §7): the
// document is an invoice or a credit memo; only a credit memo names the
// invoice it credits, a document of the same tenant and never itself; one
// naming none is allowed.
func TestCustomerCreditMemoRules(t *testing.T) {
	need(t)
	const doc = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, reference_invoice_id)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, '2026-03-10', 'EUR', 100, 0, 100, 'draft', $1, $1, $6)`
	type scene struct{ tenant, company, invoice, memo uuid.UUID }
	var none *uuid.UUID
	invoice := func(s scene) stmt { return step(doc, s.tenant, s.invoice, "CI1", s.company, "invoice", none) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a credit memo naming an invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s), step(doc, s.tenant, s.memo, "CI2", s.company, "credit_memo", &s.invoice))
		}, ""},
		{"a credit memo naming none", func(tx pgx.Tx, s scene) error {
			return all(tx, step(doc, s.tenant, s.memo, "CI2", s.company, "credit_memo", none))
		}, ""},
		{"an invoice naming an invoice", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s), step(doc, s.tenant, s.memo, "CI2", s.company, "invoice", &s.invoice))
		}, "customer_invoice_reference_on_credit"},
		{"a credit memo naming no document of the tenant", func(tx pgx.Tx, s scene) error {
			unknown := id()
			return all(tx, step(doc, s.tenant, s.memo, "CI2", s.company, "credit_memo", &unknown))
		}, "customer_invoice_reference_fkey"},
		{"a credit memo naming itself", func(tx pgx.Tx, s scene) error {
			return all(tx, step(doc, s.tenant, s.memo, "CI2", s.company, "credit_memo", &s.memo))
		}, "customer_invoice_reference_not_itself"},
		{"a reference given after entry", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s), step(doc, s.tenant, s.memo, "CI2", s.company, "credit_memo", none),
				step(`UPDATE receivables.customer_invoice SET reference_invoice_id = $2 WHERE id = $1`, s.memo, s.invoice))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), invoice: id(), memo: id()}
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

// TestCustomerInvoiceSource (T79b, migration 000296; ADR-0063 §2,
// ADR-0067 §9): an invoice names the document it was entered from by id
// and number, or neither; a document is invoiced once; the source is
// fixed with the invoice.
func TestCustomerInvoiceSource(t *testing.T) {
	need(t)
	const draft = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, source_document_id, source_document_number)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 'invoice', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $1, $1, $5, $6)`
	type scene struct{ tenant, company, doc, source uuid.UUID }
	var none *string
	var noID *uuid.UUID
	number := "BD100001"
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an invoice of a document, by id and number", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &s.source, &number))
		}, ""},
		{"two invoices a person entered, of no document", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, noID, none), step(draft, s.tenant, id(), "CI2", s.company, noID, none))
		}, ""},
		{"a source without its number", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &s.source, none))
		}, "customer_invoice_source"},
		{"a source's number without the source", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, noID, &number))
		}, "customer_invoice_source"},
		{"a source's number of 41 characters", func(tx pgx.Tx, s scene) error {
			long := strings.Repeat("1", 41)
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &s.source, &long))
		}, "customer_invoice_source_document_number_check"},
		{"two invoices of one document", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &s.source, &number), step(draft, s.tenant, id(), "CI2", s.company, &s.source, &number))
		}, "customer_invoice_source_once"},
		{"the source changed after the entry", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, &s.source, &number),
				step(`UPDATE receivables.customer_invoice SET source_document_id = gen_random_uuid(), version = version + 1`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), doc: id(), source: id()}
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

// TestCustomerInvoiceItemImmutable: the trigger itself refuses a change
// or a deletion of an item, as the owner, whom the grants do not bind,
// would make them.
func TestCustomerInvoiceItemImmutable(t *testing.T) {
	need(t)
	for name, change := range map[string]string{
		"an item changed": `UPDATE receivables.customer_invoice_item SET net_amount = 200`,
		"an item deleted": `DELETE FROM receivables.customer_invoice_item`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, doc := id(), id()
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
				t.Fatal(err)
			}
			err = all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
					currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
					VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $1, $1)`, tenant, doc),
				step(`INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, account_id, net_amount, tax_amount)
					VALUES ($1, $2, 1, gen_random_uuid(), 100, 0)`, tenant, doc),
				step(change))
			failsWith(t, err, "customer_invoice_item is insert-only")
		})
	}
}

// TestCustomerInvoiceIsTheTenants: another tenant sees neither a customer
// invoice nor its items.
func TestCustomerInvoiceIsTheTenants(t *testing.T) {
	need(t)
	a, b, actor, doc := id(), id(), id(), id()
	mustApp(t, a, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
				currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
				VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $3, $3)`, a, doc, actor),
			step(`INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, account_id, net_amount, tax_amount)
				VALUES ($1, $2, 1, gen_random_uuid(), 100, 0)`, a, doc))
	})
	mustApp(t, b, func(tx pgx.Tx) error {
		for _, table := range []string{"customer_invoice", "customer_invoice_item"} {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM receivables.`+table).Scan(&n); err != nil || n != 0 {
				t.Errorf("another tenant's rows of %s: got %d (%v), want none", table, n, err)
			}
		}
		return nil
	})
	// A row written for another tenant is refused by the policy.
	err := asApp(t, b, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'CI2', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $2, $2)`, a, actor)
	})
	failsWith(t, err, "row-level security")
}

// TestIncomingPaymentRules (T75c, migration 000272; ADR-0063 §6): an
// incoming payment applied is written once with its items: numbered once
// per company, a positive amount, one application per receipt and per
// clearing entry, each item a non-zero amount and applied once; nothing
// of it is changed or deleted.
func TestIncomingPaymentRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 2026, $5, gen_random_uuid(), '2026-03-21', 'EUR', $6, 2026, $7, $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount)
		VALUES ($1, $2, $3, 2026, $4, $5, $6)`
	type scene struct{ tenant, company, doc, receipt, clearing, invoice uuid.UUID }
	applied := func(s scene) stmt {
		return step(header, s.tenant, s.doc, "IP1", s.company, s.receipt, 10000, s.clearing)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a payment applied to an invoice and a credit", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 13000), step(item, s.tenant, s.doc, 2, id(), 1, -3000))
		}, ""},
		{"an amount of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", s.company, s.receipt, 0, s.clearing))
		}, "incoming_payment_amount_positive"},
		{"a negative amount", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", s.company, s.receipt, -1, s.clearing))
		}, "incoming_payment_amount_positive"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(header, s.tenant, id(), "IP1", s.company, id(), 1, id()))
		}, "incoming_payment_number_once"},
		{"the same number in another company", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(header, s.tenant, id(), "IP1", id(), id(), 1, id()))
		}, ""},
		{"a receipt applied twice", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(header, s.tenant, id(), "IP2", s.company, s.receipt, 10000, id()))
		}, "incoming_payment_receipt_once"},
		{"two payments with one clearing entry", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(header, s.tenant, id(), "IP2", s.company, id(), 10000, s.clearing))
		}, "incoming_payment_entry_once"},
		{"an item of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 0))
		}, "incoming_payment_item_amount_not_zero"},
		{"an item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 0, s.invoice, 1, 10000))
		}, "incoming_payment_item_item_no_check"},
		{"an item on line 0", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 0, 10000))
		}, "incoming_payment_item_line_no_check"},
		{"an item of no payment", func(tx pgx.Tx, s scene) error {
			return all(tx, step(item, s.tenant, s.doc, 1, s.invoice, 1, 10000))
		}, "incoming_payment_item_tenant_id_incoming_payment_id_fkey"},
		{"an open item applied twice", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 10000),
				step(header, s.tenant, other, "IP2", s.company, id(), 10000, id()), step(item, s.tenant, other, 1, s.invoice, 1, 10000))
		}, "incoming_payment_item_once"},
		{"another line of the same entry", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 6000), step(item, s.tenant, s.doc, 2, s.invoice, 2, 4000))
		}, ""},
		{"a payment changed", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(`UPDATE receivables.incoming_payment SET amount = 1 WHERE id = $1`, s.doc))
		}, "permission denied"},
		{"a payment deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(`DELETE FROM receivables.incoming_payment WHERE id = $1`, s.doc))
		}, "permission denied"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 10000),
				step(`UPDATE receivables.incoming_payment_item SET amount = 1 WHERE incoming_payment_id = $1`, s.doc))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 10000),
				step(`DELETE FROM receivables.incoming_payment_item WHERE incoming_payment_id = $1`, s.doc))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), doc: id(), receipt: id(), clearing: id(), invoice: id()}
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
