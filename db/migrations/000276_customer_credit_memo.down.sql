DROP INDEX receivables.customer_invoice_by_reference;
-- The kind check restored refuses the revert while a credit memo exists:
-- the previous version knows invoices alone, and a posted memo's entry
-- stays in the ledger.
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_reference_not_itself,
  DROP CONSTRAINT customer_invoice_reference_on_credit,
  DROP CONSTRAINT customer_invoice_reference_fkey,
  DROP CONSTRAINT customer_invoice_kind_check,
  ADD CONSTRAINT customer_invoice_kind_check CHECK (kind IN ('invoice')),
  DROP COLUMN reference_invoice_id;
