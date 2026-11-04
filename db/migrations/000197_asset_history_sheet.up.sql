-- The asset history sheet's year totals (T57i; ADR-0057 §5, §9;
-- 03-data-model §3.21): per asset, ledger and fiscal year of posting, and
-- per company, ledger, fiscal year and class, the sums of the assets'
-- movements in local currency by the sheet's columns. The assets context
-- adds each posting's movements to them in the posting's transaction,
-- after the movements, the class rows first and then the asset rows, each
-- in key order (assets.keepTotals), so two postings touching the same
-- classes wait for each other and never deadlock. They are sums, never a
-- plan: the sheet reads a few rows per class (or per asset) and year
-- instead of every movement. legal_entity_id and ledger_id are the tenancy
-- and ledger contexts': no foreign key leaves the schema.
-- Expand only: two new tables, filled from the movements so far below.

-- An asset's totals of one fiscal year in one ledger. The key leads with
-- the ledger and the asset, so the sheet by asset reads an asset's years
-- in one index range. class_code and legal_entity_id are the asset's,
-- which never change (asset_fixed); only the sums are ever updated.
CREATE TABLE assets.year_total (
  tenant_id               uuid NOT NULL,
  ledger_id               uuid NOT NULL,      -- ledger context
  asset_id                uuid NOT NULL,
  fiscal_year             smallint NOT NULL,
  legal_entity_id         uuid NOT NULL,      -- tenancy context
  class_code              text NOT NULL,
  acquisitions            bigint NOT NULL DEFAULT 0,  -- acquisition movements' cost
  retired_cost            bigint NOT NULL DEFAULT 0,  -- retirement movements' cost
  transferred_cost        bigint NOT NULL DEFAULT 0,  -- transfer_in and transfer_out movements' cost
  depreciation            bigint NOT NULL DEFAULT 0,  -- depreciation movements' accumulated (runs and catch-ups)
  retired_accumulated     bigint NOT NULL DEFAULT 0,  -- retirement movements' accumulated
  transferred_accumulated bigint NOT NULL DEFAULT 0,  -- transfer_in and transfer_out movements' accumulated
  PRIMARY KEY (tenant_id, ledger_id, asset_id, fiscal_year),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id)
);
SELECT platform.secure_table('assets.year_total'::regclass);

-- A class's totals of one fiscal year in one ledger of a company: the sums
-- of its assets' year totals.
CREATE TABLE assets.class_year_total (
  tenant_id               uuid NOT NULL,
  legal_entity_id         uuid NOT NULL,      -- tenancy context
  ledger_id               uuid NOT NULL,      -- ledger context
  fiscal_year             smallint NOT NULL,
  class_code              text NOT NULL,
  acquisitions            bigint NOT NULL DEFAULT 0,
  retired_cost            bigint NOT NULL DEFAULT 0,
  transferred_cost        bigint NOT NULL DEFAULT 0,
  depreciation            bigint NOT NULL DEFAULT 0,
  retired_accumulated     bigint NOT NULL DEFAULT 0,
  transferred_accumulated bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code)
);
SELECT platform.secure_table('assets.class_year_total'::regclass);

-- The tables carry no version (nothing reads them for an update), so
-- secure_table grants no update: the application may add to the sums
-- alone, never move a row to another key, and deletes nothing.
GRANT UPDATE (acquisitions, retired_cost, transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
  ON assets.year_total, assets.class_year_total TO erp_app;

-- The totals of the movements written so far. Replicas of the release
-- before keep posting while this runs, so the movements are locked SHARE
-- first: their writers wait until it commits, and both statements see the
-- same movements; a writer not waited out within the lock timeout fails
-- the migration, which the next start retries. Assets are new (T57c), so
-- the movements are few. A replica of the release before that posted
-- after this commits would write movements the totals miss; no release
-- that posts assets has been deployed (PO 2026-11-02: deployment comes
-- last), so none can.
-- Tenant rows: under a role that does not bypass row-level security this
-- fails rather than total nothing, as 000042.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;
LOCK TABLE assets.movement IN SHARE MODE;

INSERT INTO assets.year_total (tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code, acquisitions, retired_cost,
  transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
SELECT tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code,
  coalesce(sum(cost) FILTER (WHERE kind = 'acquisition'), 0), coalesce(sum(cost) FILTER (WHERE kind = 'retirement'), 0),
  coalesce(sum(cost) FILTER (WHERE kind IN ('transfer_in', 'transfer_out')), 0), coalesce(sum(accumulated) FILTER (WHERE kind = 'depreciation'), 0),
  coalesce(sum(accumulated) FILTER (WHERE kind = 'retirement'), 0),
  coalesce(sum(accumulated) FILTER (WHERE kind IN ('transfer_in', 'transfer_out')), 0)
FROM assets.movement
GROUP BY tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code;

INSERT INTO assets.class_year_total (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, acquisitions, retired_cost, transferred_cost,
  depreciation, retired_accumulated, transferred_accumulated)
SELECT tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, sum(acquisitions), sum(retired_cost), sum(transferred_cost),
  sum(depreciation), sum(retired_accumulated), sum(transferred_accumulated)
FROM assets.year_total
GROUP BY tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code;
