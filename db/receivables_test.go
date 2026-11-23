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
			return all(tx, invoice(s, s.doc, "CI1", "write_off", 10000, 1900, 11900, "draft"))
		}, "customer_invoice_kind_check"},
		{"a draft debit memo (T75j, migration 000379)", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "debit_memo", 10000, 1900, 11900, "draft"), step(item, s.tenant, s.doc, 1, 10000, &code, 1900))
		}, ""},
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
		}, "only a draft is posted and a posted document reversed"},
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

// TestCustomerInvoiceItemJurisdiction (migration 000303; ADR-0063 §4 As
// built, product owner 2026-11-12): an item names the jurisdiction its
// tax is levied in only under a tax code, in the form of an ISO 3166-2
// subdivision; an item without one is as before.
func TestCustomerInvoiceItemJurisdiction(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-03-10', 'AED', 10000, 500, 10500, 'draft', $3, $3)`
	const item = `INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, account_id, net_amount, tax_code, tax_amount,
		tax_jurisdiction) VALUES ($1, $2, 1, gen_random_uuid(), 10000, $3, $4, $5)`
	var none *string
	code, dubai, country, name, empty := "S5", "AE-DU", "AE", "Dubai", ""
	for _, c := range []struct {
		name         string
		code         *string
		tax          int64
		jurisdiction *string
		want         string // empty: allowed
	}{
		{"an emirate under a tax code", &code, 500, &dubai, ""},
		{"a tax code without a jurisdiction", &code, 500, none, ""},
		{"neither", none, 0, none, ""},
		{"a jurisdiction without a tax code", none, 0, &dubai, "customer_invoice_item_tax_jurisdiction"},
		{"a country, not a subdivision", &code, 500, &country, "customer_invoice_item_tax_jurisdiction"},
		{"a name, not a code", &code, 500, &name, "customer_invoice_item_tax_jurisdiction"},
		{"an empty jurisdiction", &code, 500, &empty, "customer_invoice_item_tax_jurisdiction"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(header, tenant, doc, id()), step(item, tenant, doc, c.code, c.tax, c.jurisdiction))
			})
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
		}, "only a draft is posted and a posted document reversed"},
		{"posted twice", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), posted(s), posted(s))
		}, "only a draft is posted and a posted document reversed"},
		{"a posted invoice moved back to draft", func(tx pgx.Tx, s scene) error {
			return all(tx, step(draft, s.tenant, s.doc, "CI1", s.company, none), posted(s),
				step(`UPDATE receivables.customer_invoice SET status = 'draft', journal_entry_year = NULL, journal_entry_id = NULL, net_due_date = NULL, version = version + 1`))
		}, "only a draft is posted and a posted document reversed"},
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
		{"a debit memo naming an invoice (T75j)", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s), step(doc, s.tenant, s.memo, "CI2", s.company, "debit_memo", &s.invoice))
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
		// Applied again after its payment was undone (T75i, 000364): the
		// ledger holds a receipt to one application at a time.
		{"a receipt applied twice", func(tx pgx.Tx, s scene) error {
			return all(tx, applied(s), step(header, s.tenant, id(), "IP2", s.company, s.receipt, 10000, id()))
		}, ""},
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
		// Likewise an open item (T75i, 000364).
		{"an open item applied twice", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, applied(s), step(item, s.tenant, s.doc, 1, s.invoice, 1, 10000),
				step(header, s.tenant, other, "IP2", s.company, id(), 10000, id()), step(item, s.tenant, other, 1, s.invoice, 1, 10000))
		}, ""},
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

// TestIncomingPaymentResidualRules (T75f, migration 000350; ADR-0063 §6
// As built): a payment's difference defaults to zero; an item's residual
// is its amount, its line in the clearing entry and its reason together,
// more than zero and less than the item, which the customer owed (a
// debit), with a reason of 1 to 40 and a free text of 1 to 200
// characters.
func TestIncomingPaymentResidualRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, 'IP1', gen_random_uuid(), gen_random_uuid(), 2026, gen_random_uuid(), gen_random_uuid(), '2026-03-21', 'EUR', 6000, 2026,
			gen_random_uuid(), $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
		residual_amount, residual_line_no, residual_reason, residual_reason_text) VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, $3, $4, $5, $6, $7)`
	var none *int64
	var noLine *int32
	var noText *string
	amount, line, reason, text, zeroLine := int64(4000), int32(3), "disputed", "The second visit", int32(0)
	long, longText := strings.Repeat("r", 41), strings.Repeat("t", 201)
	cases := []struct {
		name string
		args []any // amount, residual amount, line, reason, text
		want string
	}{
		{"an item paid in full", []any{int64(10000), none, noLine, noText, noText}, ""},
		{"a residual with its reason and text", []any{int64(10000), &amount, &line, &reason, &text}, ""},
		{"a residual without its line", []any{int64(10000), &amount, noLine, &reason, noText}, "incoming_payment_item_residual"},
		{"a residual without its reason", []any{int64(10000), &amount, &line, noText, noText}, "incoming_payment_item_residual"},
		{"a text without a residual", []any{int64(10000), none, noLine, noText, &text}, "incoming_payment_item_residual"},
		{"a residual of the whole item", []any{int64(4000), &amount, &line, &reason, noText}, "incoming_payment_item_residual"},
		{"a residual of a credit", []any{int64(-10000), &amount, &line, &reason, noText}, "incoming_payment_item_residual"},
		{"a reason of 41 characters", []any{int64(10000), &amount, &line, &long, noText}, "incoming_payment_item_residual_reason_length"},
		{"a text of 201 characters", []any{int64(10000), &amount, &line, &reason, &longText}, "incoming_payment_item_residual_text_length"},
		{"a residual on line 0", []any{int64(10000), &amount, &zeroLine, &reason, noText}, "incoming_payment_item_residual"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(header, tenant, doc), step(item, append([]any{tenant, doc}, c.args...)...))
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("a residual of zero", func(t *testing.T) {
		tenant, doc := id(), id()
		zero := int64(0)
		failsWith(t, asApp(t, tenant, func(tx pgx.Tx) error {
			return all(tx, step(header, tenant, doc), step(item, tenant, doc, int64(10000), &zero, &line, &reason, noText))
		}), "incoming_payment_item_residual")
	})
	t.Run("a difference by default zero", func(t *testing.T) {
		tenant, doc := id(), id()
		var difference int64 = -1
		mustApp(t, tenant, func(tx pgx.Tx) error {
			if err := all(tx, step(header, tenant, doc)); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT difference FROM receivables.incoming_payment WHERE id = $1`, doc).Scan(&difference)
		})
		if difference != 0 {
			t.Errorf("difference: got %d, want 0", difference)
		}
	})
}

// TestPaymentDifferenceKeys (T75f, migrations 000350 and 000351): the
// account determination takes the payment difference keys.
func TestPaymentDifferenceKeys(t *testing.T) {
	need(t)
	f := newOrders(t)
	for _, key := range []string{"payment_difference_expense", "payment_difference_revenue"} {
		t.Run(key, func(t *testing.T) {
			mustApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, "INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1,$2,$3,$4,$5)",
					f.tenant, f.entity, key, f.expense, f.actor)
			})
		})
	}
}

// TestPaymentResidualsRevert (T75f, migration 000350's down): the revert
// is refused while a payment left a residual item or wrote a difference
// off, whose clearing entry the ledger holds.
func TestPaymentResidualsRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000350_payment_residuals").Down
	for name, insert := range map[string]string{
		"a residual": `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			residual_amount, residual_line_no, residual_reason) VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, 10000, 4000, 3, 'disputed')`,
		"a difference": `UPDATE receivables.incoming_payment SET difference = 3 WHERE tenant_id = $1 AND id = $2`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, doc := id(), id()
			if err := all(tx, step(`ALTER TABLE receivables.incoming_payment DISABLE TRIGGER incoming_payment_immutable`),
				step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
					receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
					VALUES ($1, $2, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 6000, 2026, gen_random_uuid(), $1)`, tenant, doc),
				step(insert, tenant, doc)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "reverting would lose them")
		})
	}
}

// TestPaymentOnAccountRules (T75g, migration 000352; ADR-0063 §6 As
// built): what a payment left on account is more than zero with its line
// and account, or zero without; an item's application number is 0 or
// more; an application is numbered from 1 per payment, of an existing
// payment, applies a positive credit (consumed again once undone, T75i), left on account with
// its line or nothing, has a clearing entry of its own, and is never
// changed or deleted.
func TestPaymentOnAccountRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, on_account_amount, on_account_line_no, on_account_account_id,
		clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), 2026, gen_random_uuid(), gen_random_uuid(), '2026-03-21', 'EUR', 10000, $4, $5, $6, 2026,
			gen_random_uuid(), $1)`
	const app = `INSERT INTO receivables.incoming_payment_application (tenant_id, incoming_payment_id, application_no, posting_date, credit_entry_year,
		credit_entry_id, credit_line_no, amount, on_account_amount, on_account_line_no, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, '2026-03-22', 2026, $4, 2, $5, $6, $7, 2026, $8, $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount, application_no)
		VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, 100, $3)`
	type scene struct{ tenant, doc, credit, account uuid.UUID }
	line, noLine := int32(2), (*int32)(nil)
	noAccount := (*uuid.UUID)(nil)
	paid := func(s scene) stmt { return step(header, s.tenant, s.doc, "IP1", int64(10000), &line, &s.account) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string
	}{
		{"a payment on account applied twice over", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(4000), &line, id()),
				step(app, s.tenant, s.doc, 2, id(), int64(4000), int64(0), noLine, id()), step(item, s.tenant, s.doc, 1))
		}, ""},
		{"on account without a line", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", int64(10000), noLine, &s.account))
		}, "incoming_payment_on_account"},
		{"on account without an account", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", int64(10000), &line, noAccount))
		}, "incoming_payment_on_account"},
		{"a line with nothing on account", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", int64(0), &line, &s.account))
		}, "incoming_payment_on_account"},
		{"on account on line 0", func(tx pgx.Tx, s scene) error {
			zero := int32(0)
			return all(tx, step(header, s.tenant, s.doc, "IP1", int64(10000), &zero, &s.account))
		}, "incoming_payment_on_account"},
		{"an application of credit line 0", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(`INSERT INTO receivables.incoming_payment_application (tenant_id, incoming_payment_id, application_no, posting_date,
				credit_entry_year, credit_entry_id, credit_line_no, amount, on_account_amount, clearing_entry_year, clearing_entry_id, created_by)
				VALUES ($1, $2, 1, '2026-03-22', 2026, $3, 0, 100, 0, 2026, gen_random_uuid(), $1)`, s.tenant, s.doc, s.credit))
		}, "incoming_payment_application_credit_line_no_check"},
		{"an application leaving a negative amount", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(-1), &line, id()))
		}, "incoming_payment_application_on_account"},
		{"an application leaving its amount on line 0", func(tx pgx.Tx, s scene) error {
			zero := int32(0)
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(4000), &zero, id()))
		}, "incoming_payment_application_on_account"},
		{"negative on account", func(tx pgx.Tx, s scene) error {
			return all(tx, step(header, s.tenant, s.doc, "IP1", int64(-1), &line, &s.account))
		}, "incoming_payment_on_account"},
		{"an item of application -1", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(item, s.tenant, s.doc, -1))
		}, "incoming_payment_item_application_no_check"},
		{"an application numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 0, s.credit, int64(10000), int64(0), noLine, id()))
		}, "incoming_payment_application_application_no_check"},
		{"an application of no payment", func(tx pgx.Tx, s scene) error {
			return all(tx, step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(0), noLine, id()))
		}, "incoming_payment_application_tenant_id_incoming_payment_id_fkey"},
		{"an application of nothing", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(0), int64(0), noLine, id()))
		}, "incoming_payment_application_amount_positive"},
		{"left on account without a line", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(4000), noLine, id()))
		}, "incoming_payment_application_on_account"},
		// Consumed again after its application was undone (T75i, 000364):
		// the ledger holds a credit to one application at a time.
		{"a credit consumed twice", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(0), noLine, id()),
				step(app, s.tenant, s.doc, 2, s.credit, int64(10000), int64(0), noLine, id()))
		}, ""},
		{"two applications with one clearing entry", func(tx pgx.Tx, s scene) error {
			entry := id()
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(4000), &line, entry),
				step(app, s.tenant, s.doc, 2, id(), int64(4000), int64(0), noLine, entry))
		}, "incoming_payment_application_entry_once"},
		{"an application changed", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(0), noLine, id()),
				step(`UPDATE receivables.incoming_payment_application SET amount = 1 WHERE incoming_payment_id = $1`, s.doc))
		}, "permission denied"},
		{"an application deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, paid(s), step(app, s.tenant, s.doc, 1, s.credit, int64(10000), int64(0), noLine, id()),
				step(`DELETE FROM receivables.incoming_payment_application WHERE incoming_payment_id = $1`, s.doc))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), doc: id(), credit: id(), account: id()}
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

// TestPaymentOnAccountRevert (T75g, migration 000352's down): the revert
// is refused while a payment left anything on account.
func TestPaymentOnAccountRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000352_payment_on_account").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, on_account_amount, on_account_line_no, on_account_account_id,
			clearing_entry_year, clearing_entry_id, created_by)
			VALUES ($1, $1, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 6000, 6000, 2, $1, 2026, gen_random_uuid(), $1)`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would lose their credits and applications")
}

// TestSeveralCustomersRevert (T75h, migration 000363's down): the revert
// is refused while a payment paid an item of another customer than its
// own, and not for an item of the payment's customer.
func TestSeveralCustomersRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000363_receipt_several_customers").Down
	for _, c := range []struct {
		name string
		same bool
		want string
	}{{"another customer's item", false, "reverting would show their items as the payer's"}, {"the payer's item", true, ""}} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, payer, customer := id(), id(), id()
			if c.same {
				customer = payer
			}
			if err := all(tx, step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
					receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
					VALUES ($1, $1, 'IP1', $1, $2, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 6000, 2026, gen_random_uuid(), $1)`, tenant, payer),
				step(`INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount, customer_id)
					VALUES ($1, $1, 1, 2026, gen_random_uuid(), 1, 6000, $2)`, tenant, customer)); err != nil {
				t.Fatal(err)
			}
			err = exec(tx, down)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want the revert", err)
			}
		})
	}
}

// TestDebitMemoRevert (T75j, migration 000379's down): the revert is
// refused while a debit memo exists, the kind check it restores knowing
// invoices and credit memos alone.
func TestDebitMemoRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000379_customer_debit_memo").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
			VALUES ($1, $1, 'CI1', $1, $1, 'debit_memo', '2026-03-10', 'EUR', 100, 0, 100, 'draft', $1, $1)`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "customer_invoice_kind_check")
}

// TestCustomerMemoReversal (T75k, migration 000381; ADR-0063 As built): a
// posted credit or debit memo a person entered moves to reversed once,
// naming its reversal entry, the date on or after it posted and a reason,
// all together, and keeping what it posted with; an invoice, a memo
// another context entered and a draft are not reversed; nothing changes a
// reversed document; a reversal entry is one document's.
func TestCustomerMemoReversal(t *testing.T) {
	need(t)
	const doc = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, source_document_id, source_document_number,
		posting_date, payment_term_code, net_due_date, journal_entry_year, journal_entry_id)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, '2026-03-10', 'EUR', 100, 0, 100, $6, $1, $1, $7, $8,
		'2026-03-12', 'NT30', CASE WHEN $6 = 'draft' THEN NULL ELSE '2026-04-09'::date END,
		CASE WHEN $6 = 'draft' THEN NULL ELSE 2026 END, CASE WHEN $6 = 'draft' THEN NULL ELSE gen_random_uuid() END)`
	const reverse = `UPDATE receivables.customer_invoice SET status = 'reversed', reversal_date = $2, reversal_entry_year = $3, reversal_entry_id = $4,
		reversal_reason = $5, version = version + 1, updated_by = tenant_id WHERE id = $1`
	type scene struct{ tenant, company, doc, entry uuid.UUID }
	var noSource *uuid.UUID
	var noNumber, noReason *string
	var noDate *string
	var noYear *int16
	var noEntry *uuid.UUID
	year, reason, later, before := int16(2026), "Entered twice", "2026-03-20", "2026-03-11"
	posted := func(s scene, kind string) stmt {
		return step(doc, s.tenant, s.doc, "CI1", s.company, kind, "posted", noSource, noNumber)
	}
	reversed := func(s scene) stmt { return step(reverse, s.doc, &later, &year, &s.entry, &reason) }
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a credit memo reversed", func(tx pgx.Tx, s scene) error { return all(tx, posted(s, "credit_memo"), reversed(s)) }, ""},
		{"a debit memo reversed on the day it posted", func(tx pgx.Tx, s scene) error {
			day := "2026-03-12"
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &day, &year, &s.entry, &reason))
		}, ""},
		{"an invoice reversed", func(tx pgx.Tx, s scene) error { return all(tx, posted(s, "invoice"), reversed(s)) }, "customer_invoice_reversed"},
		{"a memo entered from another context's document reversed", func(tx pgx.Tx, s scene) error {
			source, number := id(), "BC1"
			return all(tx, step(doc, s.tenant, s.doc, "CI1", s.company, "credit_memo", "posted", &source, &number), reversed(s))
		}, "customer_invoice_reversed"},
		{"reversed before it posted", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &before, &year, &s.entry, &reason))
		}, "customer_invoice_reversed"},
		{"reversed without its entry", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &later, noYear, noEntry, &reason))
		}, "customer_invoice_reversed"},
		{"reversed without its entry's year", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &later, noYear, &s.entry, &reason))
		}, "customer_invoice_reversed"},
		{"reversed without a date", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, noDate, &year, &s.entry, &reason))
		}, "customer_invoice_reversed"},
		{"reversed without a reason", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &later, &year, &s.entry, noReason))
		}, "customer_invoice_reversed"},
		{"a reason of white space", func(tx pgx.Tx, s scene) error {
			blank := "   "
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &later, &year, &s.entry, &blank))
		}, "customer_invoice_reversed"},
		{"a reason of 201 characters", func(tx pgx.Tx, s scene) error {
			long := strings.Repeat("r", 201)
			return all(tx, posted(s, "debit_memo"), step(reverse, s.doc, &later, &year, &s.entry, &long))
		}, "customer_invoice_reversed"},
		{"a draft posted with a reversal", func(tx pgx.Tx, s scene) error {
			return all(tx, step(doc, s.tenant, s.doc, "CI1", s.company, "debit_memo", "draft", noSource, noNumber), step(`UPDATE receivables.customer_invoice
				SET status = 'posted', net_due_date = '2026-04-09', journal_entry_year = 2026, journal_entry_id = gen_random_uuid(), reversal_date = $2,
				reversal_entry_year = 2026, reversal_entry_id = $3, reversal_reason = 'x', version = version + 1 WHERE id = $1`, s.doc, later, s.entry))
		}, "customer_invoice_reversed"},
		{"a posted memo given a reversal without being reversed", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(`UPDATE receivables.customer_invoice SET reversal_reason = 'x', version = version + 1`))
		}, "only a draft is posted and a posted document reversed"},
		{"a draft reversed", func(tx pgx.Tx, s scene) error {
			return all(tx, step(doc, s.tenant, s.doc, "CI1", s.company, "debit_memo", "draft", noSource, noNumber), step(`UPDATE receivables.customer_invoice
				SET status = 'reversed', reversal_date = $2, reversal_entry_year = 2026, reversal_entry_id = $3, reversal_reason = 'x',
				net_due_date = '2026-04-09', journal_entry_year = 2026, journal_entry_id = $3, version = version + 1 WHERE id = $1`, s.doc, later, s.entry))
		}, "only a draft is posted and a posted document reversed"},
		{"reversed changing what it posted with", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), step(strings.Replace(reverse, "reversal_date = $2", "reversal_date = $2, net_due_date = '2026-05-01'", 1),
				s.doc, &later, &year, &s.entry, &reason))
		}, "only a draft is posted and a posted document reversed"},
		{"a reversed memo changed again", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), reversed(s), step(`UPDATE receivables.customer_invoice SET reversal_reason = 'other', version = version + 1`))
		}, "only a draft is posted and a posted document reversed"},
		{"a reversed memo posted again", func(tx pgx.Tx, s scene) error {
			return all(tx, posted(s, "debit_memo"), reversed(s), step(`UPDATE receivables.customer_invoice SET status = 'posted', reversal_date = NULL,
				reversal_entry_year = NULL, reversal_entry_id = NULL, reversal_reason = NULL, version = version + 1`))
		}, "only a draft is posted and a posted document reversed"},
		{"reversed without its posted entry and due date", func(tx pgx.Tx, s scene) error {
			return all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
				currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, posting_date, payment_term_code,
				reversal_date, reversal_entry_year, reversal_entry_id, reversal_reason)
				VALUES ($1, $2, 'CI1', $3, gen_random_uuid(), 'debit_memo', '2026-03-10', 'EUR', 100, 0, 100, 'reversed', $1, $1, '2026-03-12', 'NT30',
				'2026-03-20', 2026, $4, 'x')`, s.tenant, s.doc, s.company, s.entry))
		}, "customer_invoice_posted"},
		{"reversed without a payment term", func(tx pgx.Tx, s scene) error {
			return all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
				currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, posting_date, net_due_date, journal_entry_year, journal_entry_id,
				reversal_date, reversal_entry_year, reversal_entry_id, reversal_reason)
				VALUES ($1, $2, 'CI1', $3, gen_random_uuid(), 'debit_memo', '2026-03-10', 'EUR', 100, 0, 100, 'reversed', $1, $1, '2026-03-12', '2026-04-09',
				2026, gen_random_uuid(), '2026-03-20', 2026, $4, 'x')`, s.tenant, s.doc, s.company, s.entry))
		}, "customer_invoice_posted"},
		{"two memos reversed by one entry", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, posted(s, "debit_memo"), step(doc, s.tenant, other, "CI2", s.company, "credit_memo", "posted", noSource, noNumber), reversed(s),
				step(reverse, other, &later, &year, &s.entry, &reason))
		}, "customer_invoice_reversal_once"},
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

// TestCustomerMemoReversalRevert (T75k, migration 000381's down): the
// revert is refused while a document was reversed.
func TestCustomerMemoReversalRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000381_customer_memo_reversal").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, posting_date, payment_term_code, net_due_date,
			journal_entry_year, journal_entry_id, reversal_date, reversal_entry_year, reversal_entry_id, reversal_reason)
			VALUES ($1, $1, 'CI1', $1, $1, 'debit_memo', '2026-03-10', 'EUR', 100, 0, 100, 'reversed', $1, $1, '2026-03-12', 'NT30', '2026-04-09',
			2026, gen_random_uuid(), '2026-03-12', 2026, gen_random_uuid(), 'Entered twice')`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "000381 cannot be reverted")
}

// TestIncomingPaymentTranslationRules (T75n, migration 000398; ADR-0063 §6
// As built): an item translated into the payment's currency has its own
// currency, a known one, its own amount with the sign of its amount in
// the payment's currency, and a rate above zero, all three or none.
func TestIncomingPaymentTranslationRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, 'IP1', gen_random_uuid(), gen_random_uuid(), 2026, gen_random_uuid(), gen_random_uuid(), '2026-03-21', 'GBP', 10000, 2026,
			gen_random_uuid(), $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
		item_currency, item_amount, exchange_rate) VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, $3, $4, $5, $6)`
	var noCurrency, noRate *string
	var noAmount *int64
	eur, unknown, rate, zero, negative := "EUR", "XXQ", "0.833333333333", "0", "-1"
	own, credit := int64(12000), int64(-12000)
	for _, c := range []struct {
		name string
		args []any // amount, item currency, item amount, rate
		want string
	}{
		{"an item in the payment's currency", []any{int64(10000), noCurrency, noAmount, noRate}, ""},
		{"an item translated", []any{int64(10000), &eur, &own, &rate}, ""},
		{"a credit translated", []any{int64(-10000), &eur, &credit, &rate}, ""},
		{"a currency without its amount", []any{int64(10000), &eur, noAmount, &rate}, "incoming_payment_item_translation"},
		{"an amount without its rate", []any{int64(10000), &eur, &own, noRate}, "incoming_payment_item_translation"},
		{"a rate without its currency", []any{int64(10000), noCurrency, noAmount, &rate}, "incoming_payment_item_translation"},
		{"an amount of the other sign", []any{int64(10000), &eur, &credit, &rate}, "incoming_payment_item_translation"},
		{"a rate of zero", []any{int64(10000), &eur, &own, &zero}, "incoming_payment_item_translation"},
		{"a negative rate", []any{int64(10000), &eur, &own, &negative}, "incoming_payment_item_translation"},
		{"an unknown currency", []any{int64(10000), &unknown, &own, &rate}, "incoming_payment_item_item_currency_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(header, tenant, doc), step(item, append([]any{tenant, doc}, c.args...)...))
			})
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

// TestIncomingPaymentTranslationRevert (T75n, migration 000398's down):
// the revert is refused while a payment holds an item it translated.
func TestIncomingPaymentTranslationRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000398_incoming_payment_item_translation").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
			VALUES ($1, $1, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'GBP', 6000, 2026, gen_random_uuid(), $1)`, tenant),
		step(`INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			item_currency, item_amount, exchange_rate) VALUES ($1, $1, 1, 2026, gen_random_uuid(), 1, 6000, 'EUR', 7200, 1.2)`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would show their amounts as in the payment's currency")
}

// TestIncomingPaymentWriteOffRules (T75o, migration 000399; ADR-0063 §6 As
// built): an item's write-off is its amount, its line in the clearing
// entry, its account and its reason together, more than zero and less than
// the item, which the customer owed (a debit), never beside a residual,
// with a reason of 1 to 40 and a free text of 1 to 200 characters.
func TestIncomingPaymentWriteOffRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, 'IP1', gen_random_uuid(), gen_random_uuid(), 2026, gen_random_uuid(), gen_random_uuid(), '2026-03-21', 'EUR', 9600, 2026,
			gen_random_uuid(), $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
		write_off_amount, write_off_line_no, write_off_account_id, write_off_reason, write_off_reason_text, residual_amount, residual_line_no, residual_reason)
		VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	var none *int64
	var noLine *int32
	var noText *string
	var noAccount *uuid.UUID
	amount, line, account, reason, text, zeroLine, zero := int64(400), int32(3), id(), "bank_charges", "Charged by the customer's bank", int32(0), int64(0)
	residual, residualLine, residualReason := int64(100), int32(4), "disputed"
	long, longText, empty := strings.Repeat("r", 41), strings.Repeat("t", 201), ""
	cases := []struct {
		name string
		args []any // amount; write-off amount, line, account, reason, text; residual amount, line, reason
		want string
	}{
		{"an item paid in full", []any{int64(10000), none, noLine, noAccount, noText, noText, none, noLine, noText}, ""},
		{"a write-off with its reason and text", []any{int64(10000), &amount, &line, &account, &reason, &text, none, noLine, noText}, ""},
		{"a write-off without its line", []any{int64(10000), &amount, noLine, &account, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off without its account", []any{int64(10000), &amount, &line, noAccount, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off without its reason", []any{int64(10000), &amount, &line, &account, noText, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a text without a write-off", []any{int64(10000), none, noLine, noAccount, noText, &text, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off of zero", []any{int64(10000), &zero, &line, &account, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off of the whole item", []any{int64(400), &amount, &line, &account, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off of a credit", []any{int64(-10000), &amount, &line, &account, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off on line 0", []any{int64(10000), &amount, &zeroLine, &account, &reason, noText, none, noLine, noText}, "incoming_payment_item_write_off"},
		{"a write-off beside a residual", []any{int64(10000), &amount, &line, &account, &reason, noText, &residual, &residualLine, &residualReason},
			"incoming_payment_item_write_off"},
		{"a reason of 41 characters", []any{int64(10000), &amount, &line, &account, &long, noText, none, noLine, noText}, "incoming_payment_item_write_off_reason_length"},
		{"an empty reason", []any{int64(10000), &amount, &line, &account, &empty, noText, none, noLine, noText}, "incoming_payment_item_write_off_reason_length"},
		{"an empty text", []any{int64(10000), &amount, &line, &account, &reason, &empty, none, noLine, noText}, "incoming_payment_item_write_off_text_length"},
		{"a text of 201 characters", []any{int64(10000), &amount, &line, &account, &reason, &longText, none, noLine, noText},
			"incoming_payment_item_write_off_text_length"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(header, tenant, doc), step(item, append([]any{tenant, doc}, c.args...)...))
			})
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

// TestIncomingPaymentWriteOffRevert (T75o, migration 000399's down): the
// revert is refused while an item's difference was written off by its
// reason.
func TestIncomingPaymentWriteOffRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000399_incoming_payment_write_off").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
			VALUES ($1, $1, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 9600, 2026, gen_random_uuid(), $1)`, tenant),
		step(`INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			write_off_amount, write_off_line_no, write_off_account_id, write_off_reason) VALUES ($1, $1, 1, 2026, gen_random_uuid(), 1, 10000, 400, 3, $1, 'bank_charges')`,
			tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would lose what was written off")
}

// TestIncomingPaymentCashDiscountRules (migration 000445; ADR-0063 As
// built, cash discount granted): an item's cash discount is more than zero
// and less than the item, which the customer owed (a debit) and paid in
// full: never beside a residual item or a write-off.
func TestIncomingPaymentCashDiscountRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, 'IP1', gen_random_uuid(), gen_random_uuid(), 2026, gen_random_uuid(), gen_random_uuid(), '2026-03-21', 'EUR', 9800, 2026,
			gen_random_uuid(), $1)`
	const item = `INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
		cash_discount, residual_amount, residual_line_no, residual_reason, write_off_amount, write_off_line_no, write_off_account_id, write_off_reason)
		VALUES ($1, $2, 1, 2026, gen_random_uuid(), 1, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	var none *int64
	var noLine *int32
	var noText *string
	var noAccount *uuid.UUID
	discount, zero, part, line, reason, account, writeOff := int64(200), int64(0), int64(100), int32(4), "disputed", id(), "bank_charges"
	cases := []struct {
		name string
		args []any // amount, cash discount; residual amount, line, reason; write-off amount, line, account, reason
		want string
	}{
		{"an item paid in full without one", []any{int64(10000), none, none, noLine, noText, none, noLine, noAccount, noText}, ""},
		{"a discount on an invoice paid in full", []any{int64(10000), &discount, none, noLine, noText, none, noLine, noAccount, noText}, ""},
		{"a discount of zero", []any{int64(10000), &zero, none, noLine, noText, none, noLine, noAccount, noText}, "incoming_payment_item_cash_discount"},
		{"a discount of the whole item", []any{int64(200), &discount, none, noLine, noText, none, noLine, noAccount, noText}, "incoming_payment_item_cash_discount"},
		{"a discount on a credit", []any{int64(-10000), &discount, none, noLine, noText, none, noLine, noAccount, noText}, "incoming_payment_item_cash_discount"},
		{"a discount beside a residual", []any{int64(10000), &discount, &part, &line, &reason, none, noLine, noAccount, noText}, "incoming_payment_item_cash_discount"},
		{"a discount beside a write-off", []any{int64(10000), &discount, none, noLine, noText, &part, &line, &account, &writeOff}, "incoming_payment_item_cash_discount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error {
				return all(tx, step(header, tenant, doc), step(item, append([]any{tenant, doc}, c.args...)...))
			})
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

// TestIncomingPaymentCashDiscountRevert (migration 000445's down): the
// revert is refused while an item took a cash discount.
func TestIncomingPaymentCashDiscountRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000445_incoming_payment_cash_discount").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
			VALUES ($1, $1, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-03-21', 'EUR', 9800, 2026, gen_random_uuid(), $1)`, tenant),
		step(`INSERT INTO receivables.incoming_payment_item (tenant_id, incoming_payment_id, item_no, fiscal_year, journal_entry_id, line_no, amount,
			cash_discount) VALUES ($1, $1, 1, 2026, gen_random_uuid(), 1, 10000, 200)`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would lose them")
}

// TestBadDebtReliefRules (migration 000446; ADR-0049 As built, bad debt
// relief): a relief names a customer invoice and a write-off, which it
// relieves once, with positive amounts of which the tax is a part, a tax
// code of the code form, a notice dated no later than the relief, and an
// entry of its own; it is never changed or deleted.
func TestBadDebtReliefRules(t *testing.T) {
	need(t)
	const invoice = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-01-10', 'AED', 10000, 500, 10500, 'draft', $1, $1)`
	const relief = `INSERT INTO receivables.bad_debt_relief (tenant_id, id, legal_entity_id, customer_invoice_id, write_off_entry_year,
		write_off_entry_id, write_off_entry_number, dispute_case_id, currency, local_currency, written_off_amount, written_off_amount_lc,
		tax_amount, tax_amount_lc, tax_code, customer_notified_on, posting_date, journal_entry_year, journal_entry_id, journal_entry_number, created_by)
		VALUES ($1, gen_random_uuid(), gen_random_uuid(), $2, 2026, $3, $4, gen_random_uuid(), $5, $14, $6, $7, $8, $9, $10, $11, '2026-08-10', 2026,
			$12, $13, $1)`
	type row struct {
		writeOff, entry       uuid.UUID
		number, currency      string
		local                 string
		off, offLC, tax, taxL int64
		code                  string
		notified              *string
		entryNumber           string
	}
	good := func() row {
		return row{writeOff: id(), entry: id(), number: "100001", currency: "AED", local: "AED", off: 10500, offLC: 10500, tax: 500, taxL: 500, code: "AS",
			entryNumber: "100002"}
	}
	with := func(f func(*row)) row { r := good(); f(&r); return r }
	ins := func(tenant, doc uuid.UUID, r row) stmt {
		return step(relief, tenant, doc, r.writeOff, r.number, r.currency, r.off, r.offLC, r.tax, r.taxL, r.code, r.notified, r.entry, r.entryNumber,
			r.local)
	}
	before, after := "2026-08-01", "2026-08-11"
	cases := []struct {
		name string
		run  func(tx pgx.Tx, tenant, doc uuid.UUID) error
		want string // empty: allowed
	}{
		{"a relief of an invoice", func(tx pgx.Tx, te, d uuid.UUID) error { return all(tx, step(invoice, te, d), ins(te, d, good())) }, ""},
		{"notified before it", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.notified = &before })))
		}, ""},
		{"notified after it", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.notified = &after })))
		}, "bad_debt_relief_notified_before"},
		{"of no invoice", func(tx pgx.Tx, te, d uuid.UUID) error { return all(tx, ins(te, d, good())) }, "bad_debt_relief_invoice_fkey"},
		{"a write-off relieved twice", func(tx pgx.Tx, te, d uuid.UUID) error {
			r := good()
			again := r
			again.entry = id()
			return all(tx, step(invoice, te, d), ins(te, d, r), ins(te, d, again))
		}, "bad_debt_relief_once"},
		{"an entry of two reliefs", func(tx pgx.Tx, te, d uuid.UUID) error {
			r := good()
			again := r
			again.writeOff = id()
			return all(tx, step(invoice, te, d), ins(te, d, r), ins(te, d, again))
		}, "bad_debt_relief_entry_once"},
		{"no tax", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.tax = 0 })))
		}, "bad_debt_relief_tax_check"},
		{"a negative local tax", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.taxL = -1 })))
		}, "bad_debt_relief_tax_lc_check"},
		{"tax of all that was written off", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.tax = r.off })))
		}, "bad_debt_relief_tax_within"},
		{"local tax above what was written off", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.taxL = r.offLC + 1 })))
		}, "bad_debt_relief_tax_within"},
		{"a code not of the code form", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.code = "as" })))
		}, "bad_debt_relief_tax_code_check"},
		{"an empty write-off number", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.number = "" })))
		}, "bad_debt_relief_write_off_number_check"},
		{"an entry number of 41 characters", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.entryNumber = strings.Repeat("1", 41) })))
		}, "bad_debt_relief_entry_number_check"},
		{"an unknown currency", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.currency = "XXA" })))
		}, "bad_debt_relief_currency_fkey"},
		{"an unknown local currency", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, with(func(r *row) { r.local = "XXA" })))
		}, "bad_debt_relief_local_currency_fkey"},
		// The trigger holds the owner too, past the application's grants;
		// each statement is scoped to this tenant's invoice.
		{"changed by the owner", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, good()), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE receivables.bad_debt_relief SET tax_code = 'AX' WHERE tenant_id = $1 AND customer_invoice_id = $2`, te, d))
		}, "insert-only"},
		{"deleted by the owner", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, step(invoice, te, d), ins(te, d, good()), step(`SET LOCAL ROLE erp_owner`),
				step(`DELETE FROM receivables.bad_debt_relief WHERE tenant_id = $1 AND customer_invoice_id = $2`, te, d))
		}, "insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error { return c.run(tx, tenant, doc) })
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

// TestBadDebtReliefRevert (migration 000446's down): the revert is refused
// while a relief is recorded.
func TestBadDebtReliefRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000446_bad_debt_relief").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
			VALUES ($1, $1, 'CI1', $1, $1, 'invoice', '2026-01-10', 'AED', 10000, 500, 10500, 'draft', $1, $1)`, tenant),
		step(`INSERT INTO receivables.bad_debt_relief (tenant_id, id, legal_entity_id, customer_invoice_id, write_off_entry_year, write_off_entry_id,
			write_off_entry_number, dispute_case_id, currency, local_currency, written_off_amount, written_off_amount_lc, tax_amount, tax_amount_lc,
			tax_code, posting_date, journal_entry_year, journal_entry_id, journal_entry_number, created_by)
			VALUES ($1, $1, $1, $1, 2026, gen_random_uuid(), '100001', $1, 'AED', 'AED', 10500, 10500, 500, 500, 'AS', '2026-08-10', 2026,
				gen_random_uuid(), '100002', $1)`, tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would lose them")
}

// TestBadDebtRechargeRules (migration 000452; ADR-0049 As built batch 91):
// a relief charged again names a customer invoice and an incoming payment
// and its application, which, applied or undone, charges the invoice once,
// with a recovered amount not negative, a tax that is not nothing, a local
// tax of its sign or nothing, and an entry of its own; it is never
// changed or deleted.
func TestBadDebtRechargeRules(t *testing.T) {
	need(t)
	const invoice = `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
		currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'CI1', gen_random_uuid(), gen_random_uuid(), 'invoice', '2026-01-10', 'AED', 10000, 500, 10500, 'draft', $1, $1)`
	const payment = `INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
		receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-09-01', 'AED', 10500, 2026, gen_random_uuid(), $1)`
	const recharge = `INSERT INTO receivables.bad_debt_recharge (tenant_id, id, legal_entity_id, customer_invoice_id, incoming_payment_id, application_no,
		reset, currency, local_currency, recovered_amount, tax_amount, tax_amount_lc, posting_date, journal_entry_year, journal_entry_id,
		journal_entry_number, created_by)
		VALUES ($1, gen_random_uuid(), gen_random_uuid(), $2, $2, $3, $4, $5, $6, $7, $8, $9, '2026-09-01', 2026, $10, $11, $1)`
	type row struct {
		application          int32
		reset                bool
		currency, local      string
		recovered, tax, taxL int64
		entry                uuid.UUID
		entryNumber          string
		noInvoice, noPayment bool
	}
	good := func() row {
		return row{currency: "AED", local: "AED", recovered: 10500, tax: 500, taxL: 500, entry: id(), entryNumber: "100003"}
	}
	with := func(f func(*row)) row { r := good(); f(&r); return r }
	ins := func(tenant, doc uuid.UUID, r row) stmt {
		return step(recharge, tenant, doc, r.application, r.reset, r.currency, r.local, r.recovered, r.tax, r.taxL, r.entry, r.entryNumber)
	}
	parents := func(tenant, doc uuid.UUID, r row) []stmt {
		var out []stmt
		if !r.noInvoice {
			out = append(out, step(invoice, tenant, doc))
		}
		if !r.noPayment {
			out = append(out, step(payment, tenant, doc))
		}
		return append(out, ins(tenant, doc, r))
	}
	one := func(r row) func(tx pgx.Tx, te, d uuid.UUID) error {
		return func(tx pgx.Tx, te, d uuid.UUID) error { return all(tx, parents(te, d, r)...) }
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, tenant, doc uuid.UUID) error
		want string // empty: allowed
	}{
		{"a charge of an application", one(good()), ""},
		{"charged back less by its undoing, without local tax", one(with(func(r *row) { r.reset, r.tax, r.taxL, r.recovered = true, -500, 0, 0 })), ""},
		{"of no invoice", one(with(func(r *row) { r.noInvoice = true })), "bad_debt_recharge_invoice_fkey"},
		{"of no payment", one(with(func(r *row) { r.noPayment = true })), "bad_debt_recharge_payment_fkey"},
		{"an application charged twice", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, append(parents(te, d, good()), ins(te, d, good()))...)
		}, "bad_debt_recharge_once"},
		{"an entry of two charges", func(tx pgx.Tx, te, d uuid.UUID) error {
			r := good()
			again := r
			again.application = 1
			return all(tx, append(parents(te, d, r), ins(te, d, again))...)
		}, "bad_debt_recharge_entry_once"},
		{"a negative application", one(with(func(r *row) { r.application = -1 })), "bad_debt_recharge_application_check"},
		{"a negative recovered amount", one(with(func(r *row) { r.recovered = -1 })), "bad_debt_recharge_recovered_check"},
		{"no tax", one(with(func(r *row) { r.tax = 0 })), "bad_debt_recharge_tax_check"},
		{"a local tax of the other sign", one(with(func(r *row) { r.taxL = -1 })), "bad_debt_recharge_tax_lc_sign"},
		{"an entry number of 41 characters", one(with(func(r *row) { r.entryNumber = strings.Repeat("1", 41) })), "bad_debt_recharge_entry_number_check"},
		{"an unknown currency", one(with(func(r *row) { r.currency = "XXA" })), "bad_debt_recharge_currency_fkey"},
		{"an unknown local currency", one(with(func(r *row) { r.local = "XXA" })), "bad_debt_recharge_local_currency_fkey"},
		// The trigger holds the owner too, past the application's grants;
		// each statement is scoped to this tenant's invoice.
		{"changed by the owner", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, append(parents(te, d, good()), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE receivables.bad_debt_recharge SET tax_amount = 1 WHERE tenant_id = $1 AND customer_invoice_id = $2`, te, d))...)
		}, "insert-only"},
		{"deleted by the owner", func(tx pgx.Tx, te, d uuid.UUID) error {
			return all(tx, append(parents(te, d, good()), step(`SET LOCAL ROLE erp_owner`),
				step(`DELETE FROM receivables.bad_debt_recharge WHERE tenant_id = $1 AND customer_invoice_id = $2`, te, d))...)
		}, "insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant, doc := id(), id()
			err := asApp(t, tenant, func(tx pgx.Tx) error { return c.run(tx, tenant, doc) })
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

// TestBadDebtRechargeRevert (migration 000452's down): the revert is
// refused while a relief charged again is recorded.
func TestBadDebtRechargeRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000452_bad_debt_recharge").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
			VALUES ($1, $1, 'CI1', $1, $1, 'invoice', '2026-01-10', 'AED', 10000, 500, 10500, 'draft', $1, $1)`, tenant),
		step(`INSERT INTO receivables.incoming_payment (tenant_id, id, document_number, legal_entity_id, customer_id, receipt_entry_year,
			receipt_entry_id, receipt_account_id, posting_date, currency, amount, clearing_entry_year, clearing_entry_id, created_by)
			VALUES ($1, $1, 'IP1', $1, $1, 2026, gen_random_uuid(), $1, '2026-09-01', 'AED', 10500, 2026, gen_random_uuid(), $1)`, tenant),
		step(`INSERT INTO receivables.bad_debt_recharge (tenant_id, id, legal_entity_id, customer_invoice_id, incoming_payment_id, application_no, reset,
			currency, local_currency, recovered_amount, tax_amount, tax_amount_lc, posting_date, journal_entry_year, journal_entry_id, journal_entry_number,
			created_by) VALUES ($1, $1, $1, $1, $1, 0, false, 'AED', 'AED', 10500, 500, 500, '2026-09-01', 2026, gen_random_uuid(), '100003', $1)`,
			tenant)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "reverting would lose it")
}
