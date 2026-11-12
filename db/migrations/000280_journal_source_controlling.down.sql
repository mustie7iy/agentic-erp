-- The previous version posts no controlling entry; one posted since
-- would fail the narrower check, so it is restored NOT VALID: it holds
-- for every new row and reads none of the old.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration')) NOT VALID;
