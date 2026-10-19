-- The webhook delivery worker (T50f, ADR-0041). The worker role finds the
-- due deliveries across tenants and deletes those past the 30-day
-- retention; each delivery is claimed, sent and recorded in its tenant's
-- transaction under the app role. The worker never reads a body, a URL or
-- a secret.

-- A delivery is sent in the order of its event (the outbox's occurred_at,
-- then the event id), whichever replica fanned it out first.
ALTER TABLE integration.webhook_delivery ADD COLUMN occurred_at timestamptz;
UPDATE integration.webhook_delivery SET occurred_at = created_at, version = version + 1;
ALTER TABLE integration.webhook_delivery ALTER COLUMN occurred_at SET NOT NULL;

-- `dropped`: never sent, since its subscription was disabled while it
-- waited; resuming the subscription does not bring it back.
ALTER TABLE integration.webhook_delivery DROP CONSTRAINT webhook_delivery_status_check,
  ADD CONSTRAINT webhook_delivery_status_check CHECK (status IN ('pending', 'delivered', 'dead', 'dropped'));
ALTER TABLE integration.webhook_delivery DROP CONSTRAINT webhook_delivery_attempted,
  ADD CONSTRAINT webhook_delivery_attempted CHECK (status IN ('pending', 'dropped') OR attempts >= 1);

-- failing_since is the dispatcher's bookkeeping, not a tenant's change: it
-- alone moves without the version (the ETag) and updated_at. Any other
-- change still needs the next version.
DROP TRIGGER webhook_subscription_touch ON integration.webhook_subscription;
CREATE TRIGGER webhook_subscription_touch BEFORE UPDATE ON integration.webhook_subscription FOR EACH ROW
  WHEN (NEW.version <> OLD.version OR to_jsonb(NEW) - 'failing_since' <> to_jsonb(OLD) - 'failing_since')
  EXECUTE FUNCTION platform.touch();

GRANT SELECT (tenant_id, id, subscription_id, event_id, subject, occurred_at, status, next_attempt_at, created_at), DELETE
  ON integration.webhook_delivery TO erp_worker;
GRANT SELECT (tenant_id, id, status) ON integration.webhook_subscription TO erp_worker;

-- A delivery waits for the earlier pending ones of its subscription and
-- resource (subject).
CREATE INDEX webhook_delivery_order ON integration.webhook_delivery (tenant_id, subscription_id, subject, occurred_at, event_id) WHERE status = 'pending';
-- The retention deletes by age.
CREATE INDEX webhook_delivery_created ON integration.webhook_delivery (created_at);
