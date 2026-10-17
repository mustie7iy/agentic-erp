-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000059's state by adding each one back NOT VALID.
ALTER TABLE payables.supplier_invoice_item DROP CONSTRAINT supplier_invoice_item_quantity_check;
ALTER TABLE payables.supplier_invoice_item ADD CONSTRAINT supplier_invoice_item_quantity_check CHECK (quantity >= 0) NOT VALID;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_amounts_not_negative;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_amounts_not_negative
  CHECK (gross_amount >= 0 AND tax_amount >= 0) NOT VALID;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_subsequent_on_order;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_subsequent_on_order
  CHECK (kind NOT IN ('subsequent_debit', 'subsequent_credit') OR purchase_order_id IS NOT NULL) NOT VALID;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_reference_corrects;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_reference_corrects
  CHECK (kind <> 'invoice' OR reference_invoice_id IS NULL) NOT VALID;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_not_its_own_reference;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_not_its_own_reference CHECK (reference_invoice_id <> id) NOT VALID;
ALTER TABLE payables.supplier_invoice DROP CONSTRAINT supplier_invoice_reference_fkey;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_reference_fkey
  FOREIGN KEY (tenant_id, reference_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id) NOT VALID;
