REVOKE UPDATE (status, posting_date, payment_term_code, net_due_date, journal_entry_year, journal_entry_id, version, updated_by)
  ON receivables.customer_invoice FROM erp_app;
DROP TRIGGER customer_invoice_posted_once ON receivables.customer_invoice;
DROP FUNCTION receivables.check_customer_invoice_change();
DROP INDEX receivables.customer_invoice_entry_once;
-- The status check restored refuses the revert while a posted invoice
-- exists: the previous version knows drafts alone, and a posted invoice's
-- entry stays in the ledger.
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_posted,
  DROP CONSTRAINT customer_invoice_status_check,
  ADD CONSTRAINT customer_invoice_status_check CHECK (status IN ('draft')),
  DROP COLUMN journal_entry_id,
  DROP COLUMN journal_entry_year,
  DROP COLUMN net_due_date,
  DROP COLUMN payment_term_code;
