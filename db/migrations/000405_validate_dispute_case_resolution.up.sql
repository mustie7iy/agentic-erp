-- Validates the journal's source contexts 000404 widened NOT VALID (06
-- §6.4), one partition at a time, as 000378 validates its own.
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_entry_source_context_check', p);
  END LOOP;
END $$;
ALTER TABLE ledger.journal_entry VALIDATE CONSTRAINT journal_entry_source_context_check;
