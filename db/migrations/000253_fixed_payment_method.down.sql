-- Fixed methods do not survive the downgrade: the version before pays
-- every item by the run order. 000257's down has refused already while a
-- run holds a supplier's items in more than one payment.
DROP TABLE payments.method_exclusion;
ALTER TABLE payments.payment DROP CONSTRAINT payment_method_fixed, DROP COLUMN method_fixed;
DROP TABLE ledger.open_item_payment_method;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_payment_method, DROP COLUMN payment_method;
