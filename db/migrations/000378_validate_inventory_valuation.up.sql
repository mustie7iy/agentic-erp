-- Validates the checks 000377 added NOT VALID (06 §6.4): the account
-- determination's keys, and the journal's source contexts one partition at
-- a time, as 000328 validates its own.
ALTER TABLE ledger.account_determination VALIDATE CONSTRAINT account_determination_key_check;
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_entry_source_context_check', p);
  END LOOP;
END $$;
ALTER TABLE ledger.journal_entry VALIDATE CONSTRAINT journal_entry_source_context_check;
