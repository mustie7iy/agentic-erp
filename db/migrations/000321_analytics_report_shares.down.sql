-- The previous version reads neither the shares nor the runs' companies.
ALTER TABLE analytics.report_run DROP CONSTRAINT report_run_access_whole, DROP COLUMN permissions, DROP COLUMN companies;
DROP TABLE analytics.report_share;
DROP FUNCTION analytics.check_report_share_change();
