-- Dropping the columns drops each partition's foreign keys with them.
ALTER TABLE ledger.journal_line DROP COLUMN segment_id, DROP COLUMN profit_center_id, DROP COLUMN cost_center_id;
