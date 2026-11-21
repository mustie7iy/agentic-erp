-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000423's state by adding the checks back NOT VALID.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused', 'entered', 'held')) NOT VALID,
  DROP CONSTRAINT einvoice_entry,
  ADD CONSTRAINT einvoice_entry CHECK ((entry_at IS NOT NULL) = (status IN ('entered', 'held'))
    AND (entry_at IS NULL) = (entry_fetch_operation_id IS NULL) AND (entry_at IS NULL OR entry_at >= processed_at)) NOT VALID,
  DROP CONSTRAINT einvoice_entered,
  ADD CONSTRAINT einvoice_entered CHECK ((supplier_invoice_id IS NOT NULL) = (status = 'entered')
    AND (supplier_id IS NOT NULL) = (status = 'entered')) NOT VALID,
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status IN ('refused', 'held'))) NOT VALID;
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_source_check,
  ADD CONSTRAINT supplier_invoice_source_check CHECK (source IN ('manual', 'capture', 'edi', 'migration', 'einvoice')) NOT VALID,
  DROP CONSTRAINT supplier_invoice_document_hash,
  ADD CONSTRAINT supplier_invoice_document_hash CHECK ((source IN ('capture', 'einvoice')) = (capture_sha256 IS NOT NULL)) NOT VALID;
