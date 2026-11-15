DELETE FROM identity.sod_rule WHERE permission_a IN ('analytics.watcher.read', 'analytics.watcher.write')
  OR permission_b IN ('analytics.watcher.read', 'analytics.watcher.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('analytics.watcher.read', 'analytics.watcher.write');
DELETE FROM ref.permission WHERE code IN ('analytics.watcher.read', 'analytics.watcher.write');
DROP TABLE analytics.metric_alert;
DROP TABLE analytics.metric_watcher;
DROP FUNCTION analytics.check_metric_watcher_change();
