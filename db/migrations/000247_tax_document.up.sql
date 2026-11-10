-- The tax document of a year-end adjustment (ADR-0060 §3 and question 2;
-- the UAE tax specialist's answer C10 of 2026-11-09): a posting into a
-- special period may carry a VAT code of a UAE company when it names the
-- tax invoice or tax credit note its VAT rests on, and its tax is reported
-- in the return of the period that document is dated in. The entry keeps
-- what it names: the document's kind, number and date. On a posted entry
-- the document's date is the tax date, which is what a return selects its
-- items by; a parked entry's tax date, when it has one, is the document's
-- too, and the entry is posted with the document's date either way.
--
-- Expand only (06 §6.4): nullable columns without defaults rewrite
-- nothing, and the checks are added NOT VALID, reading none of the rows
-- under the lock; 000248 validates them. The previous release never
-- writes the columns. Which entries may name a document (a special
-- period's, with a tax code, of a UAE company) is the ledger's rule
-- (ledger.special_period_tax_code, ledger.invalid_tax_document).
ALTER TABLE ledger.journal_entry
  ADD COLUMN tax_document_kind text,
  ADD COLUMN tax_document_number text,
  ADD COLUMN tax_document_date date,
  ADD CONSTRAINT journal_entry_tax_document CHECK (
    num_nonnulls(tax_document_kind, tax_document_number, tax_document_date) IN (0, 3)
    AND (tax_document_kind IS NULL OR (tax_document_kind IN ('tax_invoice', 'tax_credit_note')
      AND tax_document_number ~ '^\S(.*\S)?$' AND char_length(tax_document_number) <= 64
      AND tax_date IS NOT DISTINCT FROM tax_document_date))) NOT VALID;

ALTER TABLE ledger.parked_entry
  ADD COLUMN tax_document_kind text,
  ADD COLUMN tax_document_number text,
  ADD COLUMN tax_document_date date,
  ADD CONSTRAINT parked_entry_tax_document CHECK (
    num_nonnulls(tax_document_kind, tax_document_number, tax_document_date) IN (0, 3)
    AND (tax_document_kind IS NULL OR (tax_document_kind IN ('tax_invoice', 'tax_credit_note')
      AND tax_document_number ~ '^\S(.*\S)?$' AND char_length(tax_document_number) <= 64
      AND (tax_date IS NULL OR tax_date = tax_document_date)))) NOT VALID;
