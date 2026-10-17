-- Validates the checks 000059 added NOT VALID (T34, 06-engineering-process
-- §6.4: expand, then validate in a later migration). Validation takes only
-- a SHARE UPDATE EXCLUSIVE lock, so writes go on while existing rows are
-- scanned; a row breaking a rule fails the migration rather than staying
-- hidden. Entry has refused such rows since 000059 shipped.
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_reference_fkey;
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_not_its_own_reference;
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_reference_corrects;
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_subsequent_on_order;
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_amounts_not_negative;
ALTER TABLE payables.supplier_invoice_item VALIDATE CONSTRAINT supplier_invoice_item_quantity_check;
