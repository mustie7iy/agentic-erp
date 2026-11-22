-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000441's state by adding the check back NOT VALID.
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_stated_local_tax,
  ADD CONSTRAINT supplier_invoice_stated_local_tax CHECK ((stated_local_tax IS NULL) = (stated_local_currency IS NULL)
    AND (stated_local_tax IS NULL OR stated_local_tax >= 0)) NOT VALID;
