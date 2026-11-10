-- The checks go with their columns.
ALTER TABLE ledger.parked_entry
  DROP COLUMN tax_document_kind,
  DROP COLUMN tax_document_number,
  DROP COLUMN tax_document_date;
ALTER TABLE ledger.journal_entry
  DROP COLUMN tax_document_kind,
  DROP COLUMN tax_document_number,
  DROP COLUMN tax_document_date;
