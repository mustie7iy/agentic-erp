DELETE FROM identity.role_permission WHERE permission_code = 'payments.statement.propose';
DELETE FROM ref.permission WHERE code = 'payments.statement.propose';
DROP INDEX payments.statement_match_session;
ALTER TABLE payments.statement_match
  DROP CONSTRAINT statement_match_agent,
  DROP CONSTRAINT statement_match_agent_open,
  DROP CONSTRAINT statement_match_outcome,
  DROP COLUMN session_id,
  DROP COLUMN proposal,
  DROP COLUMN outcome;
