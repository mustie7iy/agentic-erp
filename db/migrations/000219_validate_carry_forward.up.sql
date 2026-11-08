-- Validates the checks 000218 added NOT VALID (06 §6.4), scanning the
-- tables without blocking writes.
ALTER TABLE ledger.period_status VALIDATE CONSTRAINT period_status_opened_count;
ALTER TABLE ledger.account_determination VALIDATE CONSTRAINT account_determination_key_check;
