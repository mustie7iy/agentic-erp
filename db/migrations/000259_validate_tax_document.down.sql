-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000258's state by adding the checks back NOT VALID.
ALTER TABLE ledger.journal_entry
  DROP CONSTRAINT journal_entry_tax_document,
  ADD CONSTRAINT journal_entry_tax_document CHECK (
    num_nonnulls(tax_document_kind, tax_document_number, tax_document_date) IN (0, 3)
    AND (tax_document_kind IS NULL OR (tax_document_kind IN ('tax_invoice', 'tax_credit_note')
      AND tax_document_number ~ '^\S(.*\S)?$' AND char_length(tax_document_number) <= 64
      AND tax_date IS NOT DISTINCT FROM tax_document_date))) NOT VALID;
ALTER TABLE ledger.parked_entry
  DROP CONSTRAINT parked_entry_tax_document,
  ADD CONSTRAINT parked_entry_tax_document CHECK (
    num_nonnulls(tax_document_kind, tax_document_number, tax_document_date) IN (0, 3)
    AND (tax_document_kind IS NULL OR (tax_document_kind IN ('tax_invoice', 'tax_credit_note')
      AND tax_document_number ~ '^\S(.*\S)?$' AND char_length(tax_document_number) <= 64
      AND (tax_date IS NULL OR tax_date = tax_document_date)))) NOT VALID;
