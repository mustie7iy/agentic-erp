DELETE FROM identity.sod_rule WHERE permission_a IN ('closing.variance.propose', 'closing.variance.comment')
  OR permission_b IN ('closing.variance.propose', 'closing.variance.comment');
DELETE FROM identity.role_permission WHERE permission_code IN ('closing.variance.propose', 'closing.variance.comment');
DELETE FROM ref.permission WHERE code IN ('closing.variance.propose', 'closing.variance.comment');
DROP TABLE closing.variance_citation;
DROP TABLE closing.variance_comment;
DROP FUNCTION closing.check_variance_comment();
