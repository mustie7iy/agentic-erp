-- Metric watchers and their alerts (T96a; ADR-0077 §1-§3; 03-data-model
-- §3.23), and the analytics context's outbox for MetricAlertRaised.
-- created_by is the identity context's principal: no foreign key leaves the
-- schema (02 §2.1). Expand only (06 §6.4): new tables, an outbox and two
-- permissions, which the previous version neither reads nor writes.
SELECT platform.create_event_tables('analytics');
SELECT platform.secure_table('analytics.outbox'::regclass);
SELECT platform.secure_table('analytics.inbox'::regclass);
GRANT UPDATE (published_at) ON analytics.outbox TO erp_app;
GRANT SELECT, UPDATE (published_at) ON analytics.outbox TO erp_relay;

-- A watcher (ADR-0077 §1): its creator's, on one governed metric of the
-- model, with filters in the query's form, one company (its code) or none
-- for every company the creator may read, a period grain and one
-- condition: above or below a value (a money metric's value with its
-- currency), or a change against the previous period by more than a
-- percentage. Each condition has exactly its settings. Active until its
-- creator removes it, or failed when its metric left the model; either is
-- final, and the row is kept.
CREATE TABLE analytics.metric_watcher (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  name            text NOT NULL CONSTRAINT metric_watcher_name_check CHECK (length(btrim(name)) > 0 AND length(name) <= 200),
  metric          text NOT NULL CONSTRAINT metric_watcher_metric_check CHECK (length(metric) BETWEEN 1 AND 100),
  filters         jsonb NOT NULL DEFAULT '[]' CONSTRAINT metric_watcher_filters_check CHECK (jsonb_typeof(filters) = 'array'),
  company         text CONSTRAINT metric_watcher_company_check CHECK (length(company) BETWEEN 1 AND 20),
  grain           text NOT NULL CONSTRAINT metric_watcher_grain_check CHECK (grain IN ('month', 'year')),
  condition       text NOT NULL CONSTRAINT metric_watcher_condition_check CHECK (condition IN ('above', 'below', 'change')),
  threshold       text CONSTRAINT metric_watcher_threshold_check CHECK (threshold ~ '^-?[0-9]{1,18}(\.[0-9]{1,6})?$'),
  currency        text CONSTRAINT metric_watcher_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
  percent         text CONSTRAINT metric_watcher_percent_check CHECK (percent ~ '^[0-9]{1,6}(\.[0-9]{1,4})?$'),
  status          text NOT NULL DEFAULT 'active' CONSTRAINT metric_watcher_status_check CHECK (status IN ('active', 'removed', 'failed')),
  failure_code    text CONSTRAINT metric_watcher_failure_code_check CHECK (length(failure_code) BETWEEN 1 AND 100),
  failure_detail  text CONSTRAINT metric_watcher_failure_detail_check CHECK (length(failure_detail) BETWEEN 1 AND 2000),
  version         integer NOT NULL DEFAULT 1 CONSTRAINT metric_watcher_version_check CHECK (version >= 1),
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,       -- identity context: the person whose watcher it is
  updated_at      timestamptz NOT NULL DEFAULT now(),
  updated_by      uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- above and below compare with a value, a change with a percentage; a
  -- currency goes with a value.
  CONSTRAINT metric_watcher_condition_whole CHECK (
    (condition IN ('above', 'below')) = (threshold IS NOT NULL)
    AND (condition = 'change') = (percent IS NOT NULL)
    AND (currency IS NULL OR threshold IS NOT NULL)),
  -- A failed watcher names why, and only a failed one.
  CONSTRAINT metric_watcher_failure CHECK (
    (status = 'failed') = (failure_code IS NOT NULL) AND (failure_code IS NULL) = (failure_detail IS NULL))
);
CREATE TRIGGER metric_watcher_touch BEFORE UPDATE ON analytics.metric_watcher FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A creator's watchers by name, as their list reads them.
CREATE INDEX metric_watcher_by_creator ON analytics.metric_watcher (tenant_id, created_by, name, id);
SELECT platform.secure_table('analytics.metric_watcher'::regclass);

-- A removed or failed watcher is fixed: that was its last change.
CREATE FUNCTION analytics.check_metric_watcher_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'active' THEN
    RAISE EXCEPTION 'metric watcher %: a % watcher is not changed (metric_watcher_fixed)', OLD.id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'metric_watcher_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION analytics.check_metric_watcher_change() OWNER TO erp_owner;
CREATE TRIGGER metric_watcher_fixed BEFORE UPDATE ON analytics.metric_watcher
  FOR EACH ROW EXECUTE FUNCTION analytics.check_metric_watcher_change();
-- A change sets the status alone: what a watcher says and whose it is
-- stay as set.
REVOKE UPDATE ON analytics.metric_watcher FROM erp_app;
GRANT UPDATE (status, failure_code, failure_detail, version, updated_by) ON analytics.metric_watcher TO erp_app;

-- An alert (ADR-0077 §3): one per watcher and period (the fiscal year,
-- and the fiscal month for the month grain), naming the companies whose
-- series held the condition, the permissions the metric's data is read
-- with, and a finding per series. Raised open; nothing changes it here.
CREATE TABLE analytics.metric_alert (
  tenant_id      uuid NOT NULL,
  id             uuid NOT NULL,
  watcher_id     uuid NOT NULL,
  fiscal_year    integer NOT NULL CONSTRAINT metric_alert_fiscal_year_check CHECK (fiscal_year BETWEEN 1 AND 9999),
  fiscal_period  smallint CONSTRAINT metric_alert_fiscal_period_check CHECK (fiscal_period BETWEEN 1 AND 12),
  companies      text[] NOT NULL CONSTRAINT metric_alert_companies_check CHECK (cardinality(companies) >= 1),
  permissions    text[] NOT NULL,
  findings       jsonb NOT NULL CONSTRAINT metric_alert_findings_check CHECK (jsonb_typeof(findings) = 'array' AND jsonb_array_length(findings) >= 1),
  status         text NOT NULL DEFAULT 'open' CONSTRAINT metric_alert_status_check CHECK (status IN ('open')),
  version        integer NOT NULL DEFAULT 1 CONSTRAINT metric_alert_version_check CHECK (version >= 1),
  created_at     timestamptz NOT NULL DEFAULT now(),
  created_by     uuid NOT NULL,        -- identity context: the creator the evaluation ran as
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT metric_alert_watcher_fkey FOREIGN KEY (tenant_id, watcher_id) REFERENCES analytics.metric_watcher (tenant_id, id)
);
-- A watcher raises one alert per period, however often it is evaluated.
CREATE UNIQUE INDEX metric_alert_once ON analytics.metric_alert (tenant_id, watcher_id, fiscal_year, coalesce(fiscal_period, 0));
-- A tenant's alerts, the latest first, as their list reads them.
CREATE INDEX metric_alert_latest ON analytics.metric_alert (tenant_id, created_at DESC, id DESC);
SELECT platform.secure_table('analytics.metric_alert'::regclass);
REVOKE UPDATE ON analytics.metric_alert FROM erp_app;

-- Reading one's watchers, and setting, evaluating and removing them
-- (ADR-0077 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('analytics.watcher.read', 'View your metric watchers', false),
  ('analytics.watcher.write', 'Set, evaluate and remove your metric watchers', false);
