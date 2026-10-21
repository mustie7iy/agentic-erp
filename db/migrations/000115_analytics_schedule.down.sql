DROP TABLE analytics.extract_due;
REVOKE USAGE ON SCHEMA analytics FROM erp_relay, erp_worker;
-- Derived data (ADR-0044): pointers to digest-named manifests are dropped
-- and rebuilt by the next extract.
DELETE FROM analytics.snapshot WHERE manifest_key ~ '-[0-9a-f]{16}\.json$';
ALTER TABLE analytics.snapshot DROP CONSTRAINT snapshot_manifest_key_check;
ALTER TABLE analytics.snapshot ADD CONSTRAINT snapshot_manifest_key_check
  CHECK (manifest_key = 'analytics/v1/tenant=' || tenant_id || '/manifest-' || lpad(watermark::text, 20, '0') || '.json');
ALTER TABLE analytics.snapshot DROP COLUMN checked_at;
