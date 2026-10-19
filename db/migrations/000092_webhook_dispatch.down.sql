DROP INDEX integration.webhook_delivery_created;
DROP INDEX integration.webhook_delivery_order;
DROP INDEX integration.webhook_delivery_subscription;
REVOKE SELECT (tenant_id, id, status) ON integration.webhook_subscription FROM erp_worker;
REVOKE SELECT (tenant_id, id, subscription_id, event_id, type, subject, occurred_at, status, next_attempt_at, created_at), DELETE
  ON integration.webhook_delivery FROM erp_worker;
DROP TRIGGER webhook_subscription_touch ON integration.webhook_subscription;
CREATE TRIGGER webhook_subscription_touch BEFORE UPDATE ON integration.webhook_subscription FOR EACH ROW EXECUTE FUNCTION platform.touch();
DELETE FROM integration.webhook_delivery WHERE status = 'dropped' OR type = 'webhook.ping';
ALTER TABLE integration.webhook_delivery DROP CONSTRAINT webhook_delivery_type_check,
  ADD CONSTRAINT webhook_delivery_type_check CHECK (type ~ '^com\.example\.erp\.[a-z_]+\.[a-z_]+\.v[0-9]+$');
ALTER TABLE integration.webhook_delivery DROP CONSTRAINT webhook_delivery_attempted,
  ADD CONSTRAINT webhook_delivery_attempted CHECK (status = 'pending' OR attempts >= 1);
ALTER TABLE integration.webhook_delivery DROP CONSTRAINT webhook_delivery_status_check,
  ADD CONSTRAINT webhook_delivery_status_check CHECK (status IN ('pending', 'delivered', 'dead'));
ALTER TABLE integration.webhook_delivery DROP COLUMN occurred_at;
