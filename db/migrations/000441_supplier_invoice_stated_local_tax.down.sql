-- Invoices keeping a stated local tax, which the previous version would
-- post converted instead, are refused rather than dropped (06 §6.4: a down
-- migration loses no data). The guard reads every tenant's rows, under a
-- role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM payables.supplier_invoice WHERE stated_local_tax IS NOT NULL) THEN
    RAISE EXCEPTION 'supplier invoices keeping their stated local tax exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_stated_local_tax, DROP COLUMN stated_local_currency, DROP COLUMN stated_local_tax;
