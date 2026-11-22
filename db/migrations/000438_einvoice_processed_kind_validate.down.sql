-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000437's state by adding the check back NOT VALID.
ALTER TABLE einvoicing.einvoice
  DROP CONSTRAINT einvoice_read_processed,
  ADD CONSTRAINT einvoice_read_processed CHECK (processed_at IS NULL OR status = 'refused' OR document_kind IS NOT NULL) NOT VALID;
