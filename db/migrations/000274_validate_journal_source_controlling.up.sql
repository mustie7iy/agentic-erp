-- Validates the check 000273 added NOT VALID to the journal entries (06
-- §6.4), one partition at a time: VALIDATE scans a partition under a lock
-- that lets postings go on. The parent's check is then validated, which
-- reads no partition again: each holds it validated already.
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_entry_source_context_check', p);
  END LOOP;
END $$;
ALTER TABLE ledger.journal_entry VALIDATE CONSTRAINT journal_entry_source_context_check;
