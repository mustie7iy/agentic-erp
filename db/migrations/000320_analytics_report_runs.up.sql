-- Scheduled runs of a saved report (T95b; ADR-0076 §4-§6; 03-data-model
-- §3.23): a report's schedule, the runs the worker keeps, and the
-- analytics context's outbox for ReportRunCompleted. run_as is the
-- identity context's principal: no foreign key leaves the schema (02 §2.1).
--
-- Expand only (06 §6.4): nullable columns, a table and an outbox the
-- previous version neither reads nor writes.
SELECT platform.create_event_tables('analytics');
SELECT platform.secure_table('analytics.outbox'::regclass);
SELECT platform.secure_table('analytics.inbox'::regclass);
GRANT UPDATE (published_at) ON analytics.outbox TO erp_app;
GRANT SELECT, UPDATE (published_at) ON analytics.outbox TO erp_relay;

-- A report's schedule (ADR-0076 §4): a frequency, the company whose time
-- zone dates its runs, the weekday of a weekly one and the day of a
-- monthly one (1 to 28, which every month has), and the company's date
-- it was set on, from which runs are due. All of them or none.
ALTER TABLE analytics.report
  ADD COLUMN schedule_frequency text CONSTRAINT report_schedule_frequency_check CHECK (schedule_frequency IN ('daily', 'weekly', 'monthly')),
  ADD COLUMN schedule_company text CONSTRAINT report_schedule_company_check CHECK (length(schedule_company) BETWEEN 1 AND 20),
  ADD COLUMN schedule_weekday smallint CONSTRAINT report_schedule_weekday_check CHECK (schedule_weekday BETWEEN 1 AND 7),
  ADD COLUMN schedule_day smallint CONSTRAINT report_schedule_day_check CHECK (schedule_day BETWEEN 1 AND 28),
  ADD COLUMN scheduled_from date,
  ADD CONSTRAINT report_schedule_whole CHECK (
    (schedule_frequency IS NULL) = (schedule_company IS NULL)
    AND (schedule_frequency IS NULL) = (scheduled_from IS NULL)
    AND (schedule_weekday IS NOT NULL) = (schedule_frequency IS NOT DISTINCT FROM 'weekly')
    AND (schedule_day IS NOT NULL) = (schedule_frequency IS NOT DISTINCT FROM 'monthly')),
  -- A removed report has no schedule.
  ADD CONSTRAINT report_removed_unscheduled CHECK (status = 'active' OR schedule_frequency IS NULL);
GRANT UPDATE (schedule_frequency, schedule_company, schedule_weekday, schedule_day, scheduled_from) ON analytics.report TO erp_app;
-- The worker's round finds the scheduled reports across tenants.
CREATE INDEX report_scheduled ON analytics.report (tenant_id, id) WHERE schedule_frequency IS NOT NULL;
GRANT SELECT (tenant_id, id, owner_id, status, schedule_frequency) ON analytics.report TO erp_worker;

-- A run the worker kept (ADR-0076 §5): the day it is for, its outcome,
-- the principal it acted as, and for a success the key and SHA-256 of
-- its results in object storage, for a failure the problem's code and
-- detail. One per report and day; insert-only.
CREATE TABLE analytics.report_run (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  report_id       uuid NOT NULL,
  run_date        date NOT NULL,
  status          text NOT NULL CONSTRAINT report_run_status_check CHECK (status IN ('succeeded', 'failed')),
  run_as          uuid NOT NULL,       -- identity context: the report's owner
  object_key      text CONSTRAINT report_run_object_key_check CHECK (length(object_key) BETWEEN 1 AND 1024),
  sha256          bytea CONSTRAINT report_run_sha256_check CHECK (length(sha256) = 32),
  data_through    text CONSTRAINT report_run_data_through_check CHECK (length(data_through) <= 10),
  failure_code    text CONSTRAINT report_run_failure_code_check CHECK (length(failure_code) BETWEEN 1 AND 100),
  failure_detail  text CONSTRAINT report_run_failure_detail_check CHECK (length(failure_detail) BETWEEN 1 AND 2000),
  created_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT report_run_report_fkey FOREIGN KEY (tenant_id, report_id) REFERENCES analytics.report (tenant_id, id),
  -- A success names its results and no problem; a failure the reverse.
  CONSTRAINT report_run_outcome CHECK (
    (status = 'succeeded') = (object_key IS NOT NULL)
    AND (object_key IS NULL) = (sha256 IS NULL)
    AND (object_key IS NULL) = (data_through IS NULL)
    AND (status = 'failed') = (failure_code IS NOT NULL)
    AND (failure_code IS NULL) = (failure_detail IS NULL))
);
-- A report's run for a day is made once, however many workers claim it.
CREATE UNIQUE INDEX report_run_once ON analytics.report_run (tenant_id, report_id, run_date);
-- A tenant's runs, the latest day first, as their list reads them.
CREATE INDEX report_run_latest ON analytics.report_run (tenant_id, run_date DESC, id DESC);
CREATE TRIGGER report_run_immutable BEFORE UPDATE OR DELETE ON analytics.report_run
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('analytics.report_run'::regclass);
