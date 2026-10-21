-- migrate:partitioned-index
-- The copies of a posting are found from its first entry: a reversal
-- reverses every copy (T52b), a clearing clears them (T52c).
CREATE INDEX journal_entry_posting_group ON ledger.journal_entry (tenant_id, posting_group_id) WHERE posting_group_id IS NOT NULL;
