-- Alerts after each refresh, listed and acknowledged (T96b; ADR-0077 §1,
-- §4, §5; 03-data-model §3.23): the condition deviation, from the average
-- of the previous periods, and an alert's acknowledgement.
-- acknowledged_by is the identity context's principal: no foreign key
-- leaves the schema (02 §2.1). Expand only (06 §6.4): a nullable column
-- and a wider check on each table, two permissions; the previous version
-- writes nothing the new checks refuse.

-- A deviation compares with the average of the previous periods (1 to 12)
-- by more than a percentage of it.
ALTER TABLE analytics.metric_watcher
  ADD COLUMN periods smallint CONSTRAINT metric_watcher_periods_check CHECK (periods BETWEEN 1 AND 12),
  DROP CONSTRAINT metric_watcher_condition_check,
  ADD CONSTRAINT metric_watcher_condition_check CHECK (condition IN ('above', 'below', 'change', 'deviation')),
  DROP CONSTRAINT metric_watcher_condition_whole,
  ADD CONSTRAINT metric_watcher_condition_whole CHECK (
    (condition IN ('above', 'below')) = (threshold IS NOT NULL)
    AND (condition IN ('change', 'deviation')) = (percent IS NOT NULL)
    AND (condition = 'deviation') = (periods IS NOT NULL)
    AND (currency IS NULL OR threshold IS NOT NULL));

-- An alert is acknowledged once, by a person, when: all three or none.
ALTER TABLE analytics.metric_alert
  ADD COLUMN acknowledged_at timestamptz,
  ADD COLUMN acknowledged_by uuid,       -- identity context: the person who acknowledged it
  DROP CONSTRAINT metric_alert_status_check,
  ADD CONSTRAINT metric_alert_status_check CHECK (status IN ('open', 'acknowledged')),
  ADD CONSTRAINT metric_alert_acknowledged CHECK (
    (status = 'acknowledged') = (acknowledged_at IS NOT NULL) AND (acknowledged_at IS NULL) = (acknowledged_by IS NULL));

-- An acknowledged alert is fixed: acknowledging was its last change. A
-- change takes the next version, as platform.touch holds the tables that
-- have it (ADR-0008).
CREATE FUNCTION analytics.check_metric_alert_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'open' THEN
    RAISE EXCEPTION 'metric alert %: an acknowledged alert is not changed (metric_alert_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'metric_alert_fixed';
  END IF;
  IF NEW.version IS DISTINCT FROM OLD.version + 1 THEN
    RAISE EXCEPTION 'optimistic lock: analytics.metric_alert version must go from % to %', OLD.version, OLD.version + 1
      USING ERRCODE = 'serialization_failure';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION analytics.check_metric_alert_change() OWNER TO erp_owner;
CREATE TRIGGER metric_alert_fixed BEFORE UPDATE ON analytics.metric_alert
  FOR EACH ROW EXECUTE FUNCTION analytics.check_metric_alert_change();
-- Acknowledging sets the status, who and when alone.
GRANT UPDATE (status, acknowledged_at, acknowledged_by, version) ON analytics.metric_alert TO erp_app;

-- Reading the alerts of one's companies, and acknowledging them (ADR-0077
-- §5, §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('analytics.alert.read', 'View the metric alerts of the companies you may access', false),
  ('analytics.alert.acknowledge', 'Acknowledge metric alerts', false);
