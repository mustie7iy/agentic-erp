DROP TRIGGER journal_line_assignment ON ledger.journal_line;
DROP FUNCTION ledger.check_line_assignment();
-- Dropping the columns drops each partition's foreign keys with them.
ALTER TABLE ledger.journal_line DROP COLUMN segment_id, DROP COLUMN profit_center_id, DROP COLUMN cost_center_id;
