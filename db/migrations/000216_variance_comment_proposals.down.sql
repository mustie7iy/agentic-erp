DELETE FROM identity.sod_rule WHERE permission_a = 'cash.variance.propose' OR permission_b = 'cash.variance.propose';
DELETE FROM identity.role_permission WHERE permission_code = 'cash.variance.propose';
DELETE FROM ref.permission WHERE code = 'cash.variance.propose';
REVOKE UPDATE ON cash.variance_comment FROM erp_app;
DROP TRIGGER variance_comment_decision ON cash.variance_comment;
DROP FUNCTION cash.check_variance_comment_decision();
-- The agent's proposals go with the columns that tell them apart: what
-- the previous version reads is a person's comment.
DELETE FROM cash.variance_comment WHERE session_id IS NOT NULL;
ALTER TABLE cash.variance_comment
  DROP CONSTRAINT variance_comment_edited,
  DROP CONSTRAINT variance_comment_version,
  DROP CONSTRAINT variance_comment_rejected,
  DROP CONSTRAINT variance_comment_citations,
  DROP CONSTRAINT variance_comment_session,
  DROP COLUMN version,
  DROP COLUMN proposed_text,
  DROP COLUMN decision_reason,
  DROP COLUMN citations,
  DROP COLUMN session_id;
