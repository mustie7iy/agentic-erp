-- Credit memos citing invoices or keeping their reason, which the previous
-- version cannot hold, are refused rather than dropped (06 §6.4: a down
-- migration loses no data). The guard reads every tenant's rows, under a
-- role that row-level security binds too.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM payables.supplier_invoice_citation)
      OR EXISTS (SELECT FROM payables.supplier_invoice WHERE credit_reason_code IS NOT NULL) THEN
    RAISE EXCEPTION 'credit memos citing invoices or keeping their reason exist; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE payables.supplier_invoice_citation;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_credit_reason, DROP COLUMN credit_reason_text, DROP COLUMN credit_reason_code;
