package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReceivablesPermissions (T75a, migration 000265; ADR-0063 §8): the
// customer invoice permissions are seeded and are no approval permissions.
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
	if got, want := strings.Join(codes, " "), "receivables.invoice.post receivables.invoice.read receivables.invoice.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestCustomerInvoiceRules (T75a, migration 000265; ADR-0063 §2-§4): a
// customer invoice is entered as a draft invoice, numbered once per
// company, its amounts not negative and its gross its net and tax; an
// item is a positive net amount with tax only under a tax code; what was
// entered is fixed and never deleted.
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
		{"entered in another status", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "invoice", 10000, 1900, 11900, "posted"))
		}, "customer_invoice_status_check"},
		{"of another kind", func(tx pgx.Tx, s scene) error {
			return all(tx, invoice(s, s.doc, "CI1", "credit_memo", 10000, 1900, 11900, "draft"))
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
		{"an invoice changed", func(tx pgx.Tx, s scene) error {
			return all(tx, entered(s), step(`UPDATE receivables.customer_invoice SET posting_date = '2026-03-11', version = version + 1`))
		}, "permission denied"},
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
