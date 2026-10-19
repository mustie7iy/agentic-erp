-- Attaches 000096's index as a deferrable unique constraint, checked at
-- commit, so one configuration may swap two banks' accounts (ADR-0038).
-- Attaching takes a brief lock and no scan.
ALTER TABLE payments.house_bank ADD CONSTRAINT house_bank_account_once UNIQUE USING INDEX house_bank_account_once DEFERRABLE INITIALLY DEFERRED;
