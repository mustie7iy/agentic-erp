-- The webhook delivery worker (T50f, ADR-0041). The worker role finds the
-- due deliveries across tenants and deletes those past the 30-day
-- retention; each delivery is claimed, sent and recorded in its tenant's
-- transaction under the app role. The worker never reads a body, a URL or
-- a secret.
GRANT SELECT (tenant_id, id, subscription_id, subject, status, next_attempt_at, created_at), DELETE
  ON integration.webhook_delivery TO erp_worker;
GRANT SELECT (tenant_id, id, status) ON integration.webhook_subscription TO erp_worker;

-- A delivery waits for the earlier pending ones of its subscription and
-- resource (subject).
CREATE INDEX webhook_delivery_order ON integration.webhook_delivery (tenant_id, subscription_id, subject, id) WHERE status = 'pending';
-- The retention deletes by age.
CREATE INDEX webhook_delivery_created ON integration.webhook_delivery (created_at);
