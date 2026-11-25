-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000469's state by adding the checks back NOT VALID.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_status_check,
  ADD CONSTRAINT einvoice_status_check CHECK (status IN ('received', 'refused', 'entered', 'held', 'dismissed')) NOT VALID,
  DROP CONSTRAINT einvoice_entry,
  ADD CONSTRAINT einvoice_entry CHECK ((entry_at IS NOT NULL) = (status IN ('entered', 'held', 'dismissed'))
    AND (entry_at IS NULL) = (entry_fetch_operation_id IS NULL) AND (entry_at IS NULL OR entry_at >= processed_at)) NOT VALID,
  DROP CONSTRAINT einvoice_problem,
  ADD CONSTRAINT einvoice_problem CHECK ((problem_code IS NULL) = (problem_detail IS NULL)
    AND (problem_code IS NOT NULL) = (status IN ('refused', 'held', 'dismissed'))) NOT VALID,
  DROP CONSTRAINT einvoice_resolved,
  ADD CONSTRAINT einvoice_resolved CHECK ((resolution IS NULL) = (resolved_at IS NULL)
    AND (resolved_at IS NULL OR (entry_at IS NOT NULL AND resolved_at >= entry_at))
    AND (resolution IS NULL OR (resolution = 'retried' AND status IN ('entered', 'held')) OR (resolution = 'linked' AND status = 'entered')
      OR (resolution = 'dismissed' AND status = 'dismissed'))
    AND (status <> 'dismissed' OR resolution = 'dismissed')) NOT VALID,
  DROP CONSTRAINT einvoice_dismissal_reason,
  ADD CONSTRAINT einvoice_dismissal_reason CHECK ((dismissal_reason IS NOT NULL) = (status = 'dismissed')
    AND (dismissal_reason IS NULL OR (length(btrim(dismissal_reason)) > 0 AND length(dismissal_reason) <= 500))) NOT VALID;
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_document_hash,
  ADD CONSTRAINT supplier_invoice_document_hash CHECK ((source NOT IN ('capture', 'einvoice') OR capture_sha256 IS NOT NULL)
    AND (source IN ('capture', 'einvoice', 'manual') OR capture_sha256 IS NULL)) NOT VALID;
