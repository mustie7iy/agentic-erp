DELETE FROM identity.role_permission WHERE permission_code IN ('payments.statement.post', 'payments.statement.clearing.approve');
DELETE FROM ref.permission WHERE code IN ('payments.statement.post', 'payments.statement.clearing.approve');
DROP INDEX payments.statement_match_journal_entry;
ALTER TABLE payments.statement_match
  DROP CONSTRAINT statement_match_journal,
  DROP CONSTRAINT statement_match_posted,
  DROP CONSTRAINT statement_match_posting_date,
  DROP CONSTRAINT statement_match_posting_reason,
  DROP COLUMN posting_date,
  DROP COLUMN posting_reason,
  DROP COLUMN approval_request_id,
  DROP COLUMN journal_entry_year,
  DROP COLUMN journal_entry_id;
