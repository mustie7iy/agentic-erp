DROP TRIGGER supplier_invoice_item_quantity_kind ON payables.supplier_invoice_item;
DROP FUNCTION payables.check_item_quantity();
ALTER TABLE payables.supplier_invoice_item DROP CONSTRAINT supplier_invoice_item_quantity_check;
ALTER TABLE payables.supplier_invoice_item ADD CONSTRAINT supplier_invoice_item_quantity_check CHECK (quantity > 0);
DROP TRIGGER supplier_invoice_kind_fixed ON payables.supplier_invoice;
DROP FUNCTION payables.check_kind_fixed();
DROP INDEX payables.supplier_invoice_by_reference;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_amounts_positive,
  DROP CONSTRAINT supplier_invoice_subsequent_on_order, DROP CONSTRAINT supplier_invoice_not_its_own_reference,
  DROP CONSTRAINT supplier_invoice_reference_fkey, DROP COLUMN reference_invoice_id, DROP COLUMN kind;
