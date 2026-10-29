package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Quantities SAP invoiced before migration (T72b2, migration 000166): an
// item of a taken-over document invoices or credits a positive quantity of
// an order item, names its source document, is taken over once per
// document and item, and never changes (no grant, and insert-only).

func insertMigrated(tx pgx.Tx, f fixture, doc, po uuid.UUID, item int, reference, kind, quantity string) error {
	return exec(tx, `INSERT INTO payables.migrated_invoice_item (tenant_id, migrated_invoice_id, item_no, reference, kind, purchase_order_id,
		po_item_no, quantity, created_by) VALUES ($1, $2, $3, $4, $5, $6, 1, $7::numeric, $8)`, f.tenant, doc, item, reference, kind, po, quantity, f.actor)
}

func TestMigratedInvoiceItemRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	price := int64(250)
	var po uuid.UUID
	doc := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if po, err = f.order(tx, &price); err != nil {
			return err
		}
		return insertMigrated(tx, f, doc, po, 1, "SAP 5100000001/2026", "invoice", "2")
	})
	for _, c := range []struct{ name, reference, kind, quantity, want string }{
		{"a credit memo's item", "SAP 5200000001/2026", "credit_memo", "1.5", ""},
		{"a quantity of 0", "SAP 5100000002/2026", "invoice", "0", "migrated_invoice_item_quantity_check"},
		{"a negative quantity", "SAP 5100000003/2026", "credit_memo", "-1", "migrated_invoice_item_quantity_check"},
		{"a subsequent debit", "SAP 5100000004/2026", "subsequent_debit", "1", "migrated_invoice_item_kind_check"},
		{"no reference", " ", "invoice", "1", "migrated_invoice_item_reference_check"},
		{"the same document item again", "SAP 5100000001/2026", "invoice", "2", "migrated_invoice_item_once"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return insertMigrated(tx, f, id(), po, 1, c.reference, c.kind, c.quantity) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	for name, sql := range map[string]string{
		"changed": `UPDATE payables.migrated_invoice_item SET quantity = 3 WHERE migrated_invoice_id = $1`,
		"deleted": `DELETE FROM payables.migrated_invoice_item WHERE migrated_invoice_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, doc) }), "permission denied")
		})
	}
}
