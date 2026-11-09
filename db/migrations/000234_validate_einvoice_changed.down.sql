-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000233's state by adding the checks back NOT VALID.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_changed_hash,
  ADD CONSTRAINT einvoice_changed_hash CHECK (length(changed_hash) = 32) NOT VALID,
  DROP CONSTRAINT einvoice_changed,
  ADD CONSTRAINT einvoice_changed CHECK ((changed_hash IS NULL) = (changed_at IS NULL)
    AND (changed_hash IS NULL) = (changed_fetch_operation_id IS NULL)
    AND changed_hash IS DISTINCT FROM sha256 AND (changed_at IS NULL OR changed_at >= received_at)) NOT VALID;
