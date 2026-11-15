DELETE FROM identity.sod_rule WHERE permission_a IN ('analytics.alert.read', 'analytics.alert.acknowledge')
  OR permission_b IN ('analytics.alert.read', 'analytics.alert.acknowledge');
DELETE FROM identity.role_permission WHERE permission_code IN ('analytics.alert.read', 'analytics.alert.acknowledge');
DELETE FROM ref.permission WHERE code IN ('analytics.alert.read', 'analytics.alert.acknowledge');
REVOKE UPDATE (status, acknowledged_at, acknowledged_by, version) ON analytics.metric_alert FROM erp_app;
DROP TRIGGER metric_alert_fixed ON analytics.metric_alert;
DROP FUNCTION analytics.check_metric_alert_change();
-- 000322's checks come back: an acknowledged alert, or a deviation
-- watcher, refuses the rollback rather than being changed by it.
ALTER TABLE analytics.metric_alert
  DROP CONSTRAINT metric_alert_acknowledged,
  DROP CONSTRAINT metric_alert_status_check,
  ADD CONSTRAINT metric_alert_status_check CHECK (status IN ('open')),
  DROP COLUMN acknowledged_by,
  DROP COLUMN acknowledged_at;
ALTER TABLE analytics.metric_watcher
  DROP CONSTRAINT metric_watcher_condition_whole,
  DROP CONSTRAINT metric_watcher_condition_check,
  ADD CONSTRAINT metric_watcher_condition_check CHECK (condition IN ('above', 'below', 'change')),
  ADD CONSTRAINT metric_watcher_condition_whole CHECK (
    (condition IN ('above', 'below')) = (threshold IS NOT NULL)
    AND (condition = 'change') = (percent IS NOT NULL)
    AND (currency IS NULL OR threshold IS NOT NULL)),
  DROP COLUMN periods;
