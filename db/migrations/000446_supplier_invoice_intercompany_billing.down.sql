-- Invoices entered from an intercompany billing, which the previous
-- version could enter again, are refused rather than dropped (06 §6.4: a
-- down migration loses no data). The guard reads every tenant's rows,
-- under a role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM payables.supplier_invoice WHERE intercompany_billing_id IS NOT NULL) THEN
    RAISE EXCEPTION 'supplier invoices entered from intercompany billings exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_intercompany_billing_edi, DROP COLUMN intercompany_billing_id;
