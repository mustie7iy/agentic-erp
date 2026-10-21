-- Analytical store (ADR-0044, T55c): the time a tenant's store was last
-- found current, and the queue of tenants whose data changed.

-- checked_at is the snapshot time of the last run that read every dataset
-- and left no journal entry above its watermark; NULL until one has. A run
-- that finds nothing new records it without writing a manifest, so a quiet
-- tenant is not taken for a stale one. Freshness is judged on it.
ALTER TABLE analytics.snapshot ADD COLUMN checked_at timestamptz;
UPDATE analytics.snapshot SET checked_at = as_of;
GRANT UPDATE (checked_at) ON analytics.snapshot TO erp_app;

-- A manifest is named by its watermark and a digest of what it lists: two
-- runs with one cutoff (an open transaction holding the snapshot horizon)
-- can see different documents and master data, so the cutoff alone does
-- not name one manifest. The name without a digest is T55b's.
ALTER TABLE analytics.snapshot DROP CONSTRAINT snapshot_manifest_key_check;
ALTER TABLE analytics.snapshot ADD CONSTRAINT snapshot_manifest_key_check
  CHECK (manifest_key ~ ('^analytics/v1/tenant=' || tenant_id || '/manifest-' || lpad(watermark::text, 20, '0') || '(-[0-9a-f]{16})?\.json$'));

-- The tenants due an extract. The outbox relay marks a tenant changed in the
-- transaction that publishes its events (outbox.Relay.Relayed), once per
-- batch rather than in every posting transaction, which would make the row
-- a lock every writer of the tenant waits on. The scheduler claims a due
-- tenant at most once per interval (started_at) and records the start of
-- the last run that finished (done_at): a change after it is still due.
CREATE TABLE analytics.extract_due (
  tenant_id   uuid NOT NULL,
  changed_at  timestamptz NOT NULL,
  started_at  timestamptz,
  done_at     timestamptz,
  PRIMARY KEY (tenant_id),
  CONSTRAINT extract_due_done_at_claim CHECK (done_at IS NULL OR done_at <= started_at)
);
SELECT platform.secure_table('analytics.extract_due');
-- The application reads its tenant's row and never queues.
REVOKE INSERT ON analytics.extract_due FROM erp_app;
GRANT USAGE ON SCHEMA analytics TO erp_relay, erp_worker;
GRANT SELECT (tenant_id, changed_at), INSERT (tenant_id, changed_at), UPDATE (changed_at) ON analytics.extract_due TO erp_relay;
GRANT SELECT, UPDATE (started_at, done_at) ON analytics.extract_due TO erp_worker;
