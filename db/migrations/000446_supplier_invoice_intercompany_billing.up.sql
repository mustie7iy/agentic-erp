-- The receiving company's supplier invoice from an intercompany billing
-- (S2i3; ADR-0082 As built S2i3; 03-data-model §3.9): the invoice names
-- the supplying company's billing (sales.intercompany_billing, 000445) it
-- was entered from, so a billing is entered once whatever its supplier
-- reference becomes while parked (supplier_invoice_billing_once, built
-- concurrently by 000448). It is entered with source edi, the only source
-- that names one. The billing is another context's row: no foreign key
-- leaves a schema (02 §2.1).
-- Expand only (06 §6.4): a nullable column the previous version neither
-- reads nor writes, and its check added NOT VALID (validated by 000447).
ALTER TABLE payables.supplier_invoice
  ADD COLUMN intercompany_billing_id uuid,
  ADD CONSTRAINT supplier_invoice_intercompany_billing_edi CHECK (intercompany_billing_id IS NULL OR source = 'edi') NOT VALID;
