package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// icBillingSQL is an intercompany billing of the scene's tenant by the
// company $3 to $4's company $5, of the goods issue $6, with its number
// $7, net $8 and tax $9 (migration 000448).
const icBillingSQL = `INSERT INTO sales.intercompany_billing (tenant_id, id, document_number, legal_entity_id, customer_id, purchase_order_id,
		purchase_order_number, receiving_legal_entity_id, issue_document_id, issue_document_number, billing_date, posting_date, currency, net_amount,
		tax_amount, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
	VALUES ($1, $2, $7, $3, $4, gen_random_uuid(), 'PO1', $5, $6, 'MD1', '2026-11-22', '2026-11-22', 'EUR', $8, $9, gen_random_uuid(), 'CI1', 2026,
		gen_random_uuid(), $1)`

// icBillingItemSQL is item $3 of billing $2 billing order item $4, net
// $5, with the tax code $6, jurisdiction $7 and tax $8.
const icBillingItemSQL = `INSERT INTO sales.intercompany_billing_item (tenant_id, billing_id, item_no, po_item_no, material_id, description, quantity,
		uom_code, unit_price, price_unit, net_amount, account_id, tax_code, tax_jurisdiction, tax_amount)
	VALUES ($1, $2, $3, $4, gen_random_uuid(), 'Widget', 10, 'EA', 300, 1, $5, gen_random_uuid(), $6, $7, $8)`

// icBilling is a billing's tenant, id and companies.
type icBilling struct{ tenant, billing, company, receiving, issue uuid.UUID }

func newICBilling() icBilling {
	return icBilling{tenant: id(), billing: id(), company: id(), receiving: id(), issue: id()}
}

func (s icBilling) row(number string, net, tax int64) stmt {
	return step(icBillingSQL, s.tenant, s.billing, s.company, id(), s.receiving, s.issue, number, net, tax)
}

// with is the scene's billing (BD1, net 3000) with one text column
// replaced.
func (s icBilling) with(column, value string) stmt {
	return step(`WITH b AS (SELECT $1::uuid AS tenant_id, $2::uuid AS id, $3::uuid AS le, $4::uuid AS recv, $5::uuid AS issue)
		INSERT INTO sales.intercompany_billing (tenant_id, id, document_number, legal_entity_id, customer_id, purchase_order_id,
			purchase_order_number, receiving_legal_entity_id, issue_document_id, issue_document_number, billing_date, posting_date, currency, net_amount,
			tax_amount, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
		SELECT tenant_id, id, 'BD1', le, gen_random_uuid(), gen_random_uuid(), CASE WHEN $6 = 'purchase_order_number' THEN $7 ELSE 'PO1' END, recv, issue,
			CASE WHEN $6 = 'issue_document_number' THEN $7 ELSE 'MD1' END, '2026-11-22', '2026-11-22', 'EUR', 3000, 0, gen_random_uuid(),
			CASE WHEN $6 = 'customer_invoice_number' THEN $7 ELSE 'CI1' END, 2026, gen_random_uuid(), tenant_id FROM b`,
		s.tenant, s.billing, s.company, s.receiving, s.issue, column, value)
}

// withInvoice is a billing of the scene's companies with its id, goods
// issue, number and customer invoice.
func (s icBilling) withInvoice(billing, issue uuid.UUID, number string, invoice uuid.UUID) stmt {
	return step(`INSERT INTO sales.intercompany_billing (tenant_id, id, document_number, legal_entity_id, customer_id, purchase_order_id,
			purchase_order_number, receiving_legal_entity_id, issue_document_id, issue_document_number, billing_date, posting_date, currency, net_amount,
			tax_amount, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), gen_random_uuid(), 'PO1', $5, $6, 'MD1', '2026-11-22', '2026-11-22', 'EUR', 3000, 0, $7, 'CI1', 2026,
			gen_random_uuid(), $1)`, s.tenant, billing, number, s.company, s.receiving, issue, invoice)
}

func (s icBilling) item(no, po int, net int64, code, jurisdiction any, tax int64) stmt {
	return step(icBillingItemSQL, s.tenant, s.billing, no, po, net, code, jurisdiction, tax)
}

// TestIntercompanyBillingRules (S2i2, migration 000448; ADR-0082 As built
// S2i): a billing bills another company, a goods issue once, with a net
// amount above zero and a tax not below; its numbers are 1 to 40
// characters; an invoice is of one billing; an item bills an order item
// once, numbered from 1, at an amount above zero, and names a jurisdiction
// or bears tax only with a
// tax code.
func TestIntercompanyBillingRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s icBilling) []stmt
		want string // empty: allowed
	}{
		{"a billing with its items", func(s icBilling) []stmt {
			return []stmt{s.row("BD1", 3000, 150), s.item(1, 1, 2000, "S5", "AE-SH", 100), s.item(2, 2, 1000, nil, nil, 0)}
		}, ""},
		{"a billing of its own company", func(s icBilling) []stmt {
			return []stmt{step(icBillingSQL, s.tenant, s.billing, s.company, id(), s.company, s.issue, "BD1", 3000, 0)}
		}, "intercompany_billing_other_company"},
		{"a goods issue billed twice", func(s icBilling) []stmt {
			return []stmt{s.row("BD1", 3000, 0), step(icBillingSQL, s.tenant, id(), s.company, id(), s.receiving, s.issue, "BD2", 3000, 0)}
		}, "intercompany_billing_once"},
		{"a number twice", func(s icBilling) []stmt {
			return []stmt{s.row("BD1", 3000, 0), step(icBillingSQL, s.tenant, id(), s.company, id(), s.receiving, id(), "BD1", 3000, 0)}
		}, "intercompany_billing_number_once"},
		{"a number of 41 characters", func(s icBilling) []stmt { return []stmt{s.row(strings.Repeat("B", 41), 3000, 0)} },
			"intercompany_billing_document_number_check"},
		{"an order number of 41 characters", func(s icBilling) []stmt { return []stmt{s.with("purchase_order_number", strings.Repeat("P", 41))} },
			"intercompany_billing_purchase_order_number_check"},
		{"a goods issue number of 41 characters", func(s icBilling) []stmt { return []stmt{s.with("issue_document_number", strings.Repeat("M", 41))} },
			"intercompany_billing_issue_document_number_check"},
		{"an invoice number of 41 characters", func(s icBilling) []stmt { return []stmt{s.with("customer_invoice_number", strings.Repeat("C", 41))} },
			"intercompany_billing_customer_invoice_number_check"},
		{"an invoice of two billings", func(s icBilling) []stmt {
			invoice := id()
			return []stmt{s.withInvoice(s.billing, s.issue, "BD1", invoice), s.withInvoice(id(), id(), "BD2", invoice)}
		}, "intercompany_billing_invoice_once"},
		{"a net amount of zero", func(s icBilling) []stmt { return []stmt{s.row("BD1", 0, 0)} }, "intercompany_billing_net_positive"},
		{"a negative tax", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, -1)} }, "intercompany_billing_tax_not_negative"},
		{"an order item twice", func(s icBilling) []stmt {
			return []stmt{s.row("BD1", 3000, 0), s.item(1, 1, 2000, nil, nil, 0), s.item(2, 1, 1000, nil, nil, 0)}
		}, "intercompany_billing_item_once"},
		{"an item number of zero", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(0, 1, 3000, nil, nil, 0)} },
			"intercompany_billing_item_no_check"},
		{"an order item number of zero", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(1, 0, 3000, nil, nil, 0)} },
			"intercompany_billing_item_po_item_no_check"},
		{"an item of zero", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(1, 1, 0, nil, nil, 0)} },
			"intercompany_billing_item_net_positive"},
		{"an item's negative tax", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(1, 1, 3000, "S5", nil, -1)} },
			"intercompany_billing_item_tax_not_negative"},
		{"a jurisdiction without a tax code", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(1, 1, 3000, nil, "AE-SH", 0)} },
			"intercompany_billing_item_tax"},
		{"tax without a tax code", func(s icBilling) []stmt { return []stmt{s.row("BD1", 3000, 0), s.item(1, 1, 3000, nil, nil, 150)} },
			"intercompany_billing_item_tax"},
		{"an item of no billing", func(s icBilling) []stmt { return []stmt{s.item(1, 1, 3000, nil, nil, 0)} },
			"intercompany_billing_item_billing_fkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newICBilling()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			failsOn(t, err, c.want)
		})
	}
}

// TestIntercompanyBillingImmutable (S2i2, migration 000448): a billing
// and its items are never changed or deleted, by the application (no
// grant) nor, past the grants, by the owner (the insert-only triggers).
func TestIntercompanyBillingImmutable(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, c := range map[string]struct{ sql, want string }{
		"the billing changed":   {`UPDATE sales.intercompany_billing SET tax_amount = 0 WHERE id = $1`, "intercompany_billing is insert-only"},
		"the billing deleted":   {`DELETE FROM sales.intercompany_billing WHERE id = $1`, "intercompany_billing is insert-only"},
		"an item changed":       {`UPDATE sales.intercompany_billing_item SET tax_amount = 0 WHERE billing_id = $1`, "intercompany_billing_item is insert-only"},
		"an item deleted":       {`DELETE FROM sales.intercompany_billing_item WHERE billing_id = $1`, "intercompany_billing_item is insert-only"},
		"the app's update":      {`UPDATE sales.intercompany_billing SET tax_amount = 0 WHERE id = $1`, "permission denied"},
		"the app's item change": {`UPDATE sales.intercompany_billing_item SET tax_amount = 0 WHERE billing_id = $1`, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newICBilling()
			if strings.HasPrefix(name, "the app") {
				err := asApp(t, s.tenant, func(tx pgx.Tx) error {
					return all(tx, s.row("BD1", 3000, 150), s.item(1, 1, 3000, "S5", "AE-SH", 150), step(c.sql, s.billing))
				})
				failsWith(t, err, c.want)
				return
			}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.row("BD1", 3000, 150),
				s.item(1, 1, 3000, "S5", "AE-SH", 150)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, c.sql, s.billing), c.want)
		})
	}
}

// TestIntercompanyBillingDownRefused: 000448's down migration refuses to
// drop intercompany billings (06 §6.4: a down migration loses no data).
func TestIntercompanyBillingDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := newICBilling()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.row("BD1", 3000, 0),
		step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, migrationNamed(t, "000448_intercompany_billing").Down), "intercompany billings exist")
}
