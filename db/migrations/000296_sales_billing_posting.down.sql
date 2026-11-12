REVOKE UPDATE (status, posting_date, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, version, updated_by)
  ON sales.billing_document FROM erp_app;
DROP TRIGGER billing_document_posted_once ON sales.billing_document;
DROP FUNCTION sales.check_billing_document_change();
DROP INDEX sales.billing_document_invoice_once;
-- The status check restored refuses the revert while a posted billing
-- document exists: the previous version knows created ones alone, and a
-- posted document's customer invoice and entry stay where they are.
ALTER TABLE sales.billing_document
  DROP CONSTRAINT billing_document_posted,
  DROP CONSTRAINT billing_document_status_check,
  ADD CONSTRAINT billing_document_status_check CHECK (status IN ('created')),
  DROP COLUMN journal_entry_id,
  DROP COLUMN journal_entry_year,
  DROP COLUMN customer_invoice_number,
  DROP COLUMN customer_invoice_id,
  DROP COLUMN posting_date;
DROP INDEX receivables.customer_invoice_source_once;
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_source,
  DROP COLUMN source_document_number,
  DROP COLUMN source_document_id;
