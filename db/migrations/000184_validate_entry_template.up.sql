-- Validates the checks and key 000183 added to ledger.parked_entry NOT
-- VALID (06 §6.4), scanning the table without blocking writes.
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_created_by_kind;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_template_fkey;
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_occurrence;
