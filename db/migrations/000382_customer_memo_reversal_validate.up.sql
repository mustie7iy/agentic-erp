-- Validates what 000381 added NOT VALID (06 §6.4).
ALTER TABLE receivables.customer_invoice VALIDATE CONSTRAINT customer_invoice_status_check;
ALTER TABLE receivables.customer_invoice VALIDATE CONSTRAINT customer_invoice_posted;
ALTER TABLE receivables.customer_invoice VALIDATE CONSTRAINT customer_invoice_reversed;
