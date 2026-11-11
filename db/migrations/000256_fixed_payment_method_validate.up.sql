-- Validates the checks 000253 added NOT VALID (06 §6.4): no row written
-- before fixes a method, so both hold.
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_payment_method;
ALTER TABLE payments.payment VALIDATE CONSTRAINT payment_method_fixed;
