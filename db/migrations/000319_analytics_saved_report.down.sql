DELETE FROM identity.sod_rule WHERE permission_a IN ('analytics.report.read', 'analytics.report.write')
  OR permission_b IN ('analytics.report.read', 'analytics.report.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('analytics.report.read', 'analytics.report.write');
DELETE FROM ref.permission WHERE code IN ('analytics.report.read', 'analytics.report.write');
GRANT UPDATE ON analytics.report TO erp_app;
DROP TRIGGER report_removed_fixed ON analytics.report;
DROP FUNCTION analytics.check_report_change();
DROP INDEX analytics.report_by_owner;
-- The table goes back to 000010's shape; the previous version reads none
-- of its rows.
ALTER TABLE analytics.report
  DROP CONSTRAINT report_version_check,
  DROP CONSTRAINT report_queries_check,
  DROP CONSTRAINT report_spec_check,
  DROP CONSTRAINT report_question_check,
  DROP CONSTRAINT report_title_check,
  DROP COLUMN status;
