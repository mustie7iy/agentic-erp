DROP TABLE analytics.report_run;
REVOKE SELECT (tenant_id, id, owner_id, status, schedule_frequency) ON analytics.report FROM erp_worker;
DROP INDEX analytics.report_scheduled;
-- The schedules go with their columns; the previous version reads none.
ALTER TABLE analytics.report
  DROP CONSTRAINT report_removed_unscheduled,
  DROP CONSTRAINT report_schedule_whole,
  DROP COLUMN scheduled_from,
  DROP COLUMN schedule_day,
  DROP COLUMN schedule_weekday,
  DROP COLUMN schedule_company,
  DROP COLUMN schedule_frequency;
REVOKE SELECT, UPDATE (published_at) ON analytics.outbox FROM erp_relay;
DROP TABLE analytics.outbox, analytics.inbox;
