package db

import (
	"context"
	"testing"
)

// billingInvoiceRow inserts a supplier invoice with id $2 of the tenant
// $1, triggers off as a seed, with the source, status and intercompany
// billing $3 given, its reference $4.
func billingInvoiceRow(source, status string) string {
	return `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
		currency, gross_amount, tax_amount, status, source, intercompany_billing_id, created_by, updated_by)
		VALUES ($1, $2::uuid, 'D' || $2::uuid::text, $1, $1, $4, current_date, 'EUR', 100, 0, '` + status + `', '` + source + `', $3, $1, $1)`
}

// TestSupplierInvoiceBilling (S2i3; ADR-0082 As built S2i3, migrations
// 000446-000448): an invoice names an intercompany billing with source edi
// alone, and a billing is entered as one live invoice: a rejected,
// cancelled or reversed one frees it.
func TestSupplierInvoiceBilling(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name          string
		source        string
		earlierStatus string // "": no earlier invoice of the billing
		want          string // empty: allowed
	}{
		{"an edi invoice", "edi", "", ""},
		{"a manual one", "manual", "", "supplier_invoice_intercompany_billing_edi"},
		{"a migrated one", "migration", "", "supplier_invoice_intercompany_billing_edi"},
		{"after a parked one", "edi", "parked", "supplier_invoice_billing_once"},
		{"after one on hold", "edi", "on_hold", "supplier_invoice_billing_once"},
		{"after a cancelled one", "edi", "cancelled", ""},
		{"after a rejected one", "edi", "rejected", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, billing := id(), id()
			steps := []stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String())}
			if c.earlierStatus != "" {
				reason := ""
				if c.earlierStatus == "on_hold" {
					reason = `, hold_reason = 'Held'`
				}
				earlier := id()
				steps = append(steps, step(`SET LOCAL session_replication_role = replica`),
					step(billingInvoiceRow("edi", "parked"), tenant, earlier, billing, "BD-EARLIER"),
					step(`UPDATE payables.supplier_invoice SET status = '`+c.earlierStatus+`'`+reason+` WHERE id = $1`, earlier),
					step(`SET LOCAL session_replication_role = origin`))
			}
			steps = append(steps, step(`SET LOCAL ROLE erp_app`))
			if err := all(tx, steps...); err != nil {
				t.Fatal(err)
			}
			err = exec(tx, billingInvoiceRow(c.source, "parked"), tenant, id(), billing, "BD-1")
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("the check validated by 000447", func(t *testing.T) {
		var valid bool
		if err := testDB.QueryRow(context.Background(), `SELECT convalidated FROM pg_constraint WHERE conrelid = 'payables.supplier_invoice'::regclass
			AND conname = 'supplier_invoice_intercompany_billing_edi'`).Scan(&valid); err != nil || !valid {
			t.Errorf("got %v %v, want validated", valid, err)
		}
	})
}

// TestSupplierInvoiceBillingDownRefused: 000446's down migration refuses to
// drop the billing invoices were entered from (06 §6.4: a down migration
// loses no data).
func TestSupplierInvoiceBillingDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant := id()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
		step(billingInvoiceRow("edi", "parked"), tenant, id(), id(), "BD-1"), step(`SET LOCAL session_replication_role = origin`),
		step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, migrationNamed(t, "000446_supplier_invoice_intercompany_billing").Down), "supplier invoices entered from intercompany billings exist")
}
