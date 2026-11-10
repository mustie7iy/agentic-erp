-- Validates the checks 000247 added NOT VALID (06 §6.4), scanning the
-- tables without blocking writes.
ALTER TABLE ledger.journal_entry VALIDATE CONSTRAINT journal_entry_tax_document;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_tax_document;
