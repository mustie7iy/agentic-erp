-- The depreciation booked per asset and ledger (T57f3, ADR-0057 §5, §7):
-- minus the sum of the asset's depreciation movements in the ledger (a
-- debit of expense, so positive while depreciation is posted), kept on
-- the terms by every posting that writes depreciation movements, in the
-- posting's own transaction (keepTotals), as the year totals (000197)
-- are. A depreciation run's preview over 100,000 assets read it from the
-- year totals, some 580,000 rows a page; it now reads it with the terms
-- it already reads (01 §1.3: p95 1 s).
ALTER TABLE assets.asset_terms ADD COLUMN booked_depreciation bigint NOT NULL DEFAULT 0;

-- The depreciation booked so far. Replicas of the release before keep
-- posting while this runs, so the movements are locked SHARE first: their
-- writers wait until it commits; a writer not waited out within the lock
-- timeout fails the migration, which the next start retries, and one that
-- updated its terms before writing its movements may deadlock with the
-- update below, PostgreSQL then aborting one of the two to be retried. A replica of
-- the release before that posted after this commits would write
-- depreciation the column misses; no release that posts assets has been
-- deployed (PO 2026-11-02: deployment comes last), so none can (as
-- 000197). Tenant rows: under a role that does not bypass row-level
-- security this fails rather than total nothing, as 000042.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;
LOCK TABLE assets.movement IN SHARE MODE;

UPDATE assets.asset_terms t SET booked_depreciation = -m.accumulated, version = t.version + 1
FROM (SELECT tenant_id, asset_id, ledger_id, sum(accumulated) AS accumulated FROM assets.movement WHERE kind = 'depreciation'
  GROUP BY tenant_id, asset_id, ledger_id) m
WHERE t.tenant_id = m.tenant_id AND t.asset_id = m.asset_id AND t.ledger_id = m.ledger_id AND m.accumulated <> 0;
