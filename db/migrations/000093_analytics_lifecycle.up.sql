-- Analytical store (ADR-0044, T55f): the lifecycle of its files, its
-- rebuild after a restore, the hourly sweep and the replica wait.

-- The manifests a publication replaced, kept seven days so a running query
-- never loses a file; the lifecycle then deletes the files no newer manifest
-- lists, the manifest, and the row. A run that lost the race records its own
-- manifest here too. Bookkeeping of derived data: no audit record.
CREATE TABLE analytics.superseded (
  tenant_id      uuid NOT NULL,
  manifest_key   text NOT NULL CONSTRAINT superseded_manifest_key_check
                 CHECK (manifest_key ~ ('^analytics/v1/tenant=' || tenant_id || '/manifest-[0-9]{20}(-[0-9a-f]{16})?\.json$')),
  superseded_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, manifest_key)
);
SELECT platform.secure_table('analytics.superseded');
-- A run lists in the tenant's scope; listed again, a manifest is kept from
-- then. The application deletes nothing: the lifecycle forgets the rows it
-- has acted on as the worker.
GRANT UPDATE (superseded_at) ON analytics.superseded TO erp_app;

-- A rebuild (the database restored under the store) runs as the worker,
-- as an erasure does: it deletes the tenant's pointer and list, and the
-- next run extracts the tenant from the start.
GRANT DELETE ON analytics.snapshot, analytics.superseded TO erp_worker;
GRANT SELECT ON analytics.snapshot, analytics.superseded TO erp_worker;

-- The sweep marks tenants whose store was not found current within the
-- freshness target due an extract, so a lost event delays data and never
-- loses it.
GRANT INSERT (tenant_id, changed_at), UPDATE (changed_at) ON analytics.extract_due TO erp_worker;

-- The primary's WAL position at the claim: the run reads on the replica
-- only once it has replayed past it, else on the primary.
ALTER TABLE analytics.extract_due ADD COLUMN claimed_lsn pg_lsn;
GRANT UPDATE (claimed_lsn) ON analytics.extract_due TO erp_worker;

-- The freshness gauges (erp_analytics_lag_seconds), read as the worker:
-- the entries at or above each tenant's watermark and when they posted.
GRANT USAGE ON SCHEMA ledger TO erp_worker;
GRANT SELECT (tenant_id, posted_xid, posted_at) ON ledger.journal_entry TO erp_worker;
