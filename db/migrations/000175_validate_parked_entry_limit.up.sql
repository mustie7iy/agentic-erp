-- Validates the checks and keys 000174 added NOT VALID (06 §6.4), scanning
-- the table without blocking writes.
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_limit_kind_check;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_limit_figures;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_limit_by_service;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_limit_debits_currency_fkey;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_limit_currency_fkey;
