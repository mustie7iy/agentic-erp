-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000446's state by adding the check back NOT VALID.
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_intercompany_billing_edi,
  ADD CONSTRAINT supplier_invoice_intercompany_billing_edi CHECK (intercompany_billing_id IS NULL OR source = 'edi') NOT VALID;
