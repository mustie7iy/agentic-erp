-- The controlling context posts journal entries (T60e; ADR-0064 §3, §5:
-- an order's settlement, later the allocation cycles' runs, through
-- ledger.Postings with source_context controlling), so the journal's
-- list of source contexts gains it. Expand only (06 §6.4): the wider
-- check replaces the narrower one NOT VALID, which reads no row under the
-- lock; every row there satisfied the narrower check, and 000281
-- validates the new one partition by partition. Added to the parent, it
-- is each partition's too, and a partition created later
-- (db.EnsurePartitions) gets it validated, being empty.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling')) NOT VALID;
