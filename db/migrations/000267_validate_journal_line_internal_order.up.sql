-- Validates the check 000266 added NOT VALID to the journal lines (06 §6.4),
-- one partition at a time: VALIDATE scans a partition under a lock that
-- lets postings go on. The parent's check is then validated, which reads
-- no partition again: each holds it validated already.
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT journal_line_one_cost_object', p);
  END LOOP;
END $$;
ALTER TABLE ledger.journal_line VALIDATE CONSTRAINT journal_line_one_cost_object;
