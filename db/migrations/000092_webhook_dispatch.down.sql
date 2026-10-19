DROP INDEX integration.webhook_delivery_created;
DROP INDEX integration.webhook_delivery_order;
REVOKE SELECT (tenant_id, id, status) ON integration.webhook_subscription FROM erp_worker;
REVOKE SELECT (tenant_id, id, subscription_id, subject, status, next_attempt_at, created_at), DELETE
  ON integration.webhook_delivery FROM erp_worker;
