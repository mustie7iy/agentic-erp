-- Validates the check 000439 added NOT VALID on payables.supplier_invoice
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_credit_reason;
