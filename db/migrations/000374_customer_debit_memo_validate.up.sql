-- Validates what 000373 added NOT VALID (06 §6.4).
ALTER TABLE receivables.customer_invoice VALIDATE CONSTRAINT customer_invoice_kind_check;
