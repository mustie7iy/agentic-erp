-- Validates the check 000216 added NOT VALID (06 §6.4), scanning the table
-- without blocking writes.
ALTER TABLE ledger.parked_entry VALIDATE CONSTRAINT parked_entry_fiscal_period_special;
