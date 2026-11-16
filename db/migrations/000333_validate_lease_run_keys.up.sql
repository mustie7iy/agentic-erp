-- Validates the account determination's key check 000332 added NOT VALID
-- (06 §6.4).
ALTER TABLE ledger.account_determination VALIDATE CONSTRAINT account_determination_key_check;
