-- The asset history sheet's class totals per fiscal period (po:2026-11-09,
-- B14; ADR-0057 §9 and As built, the consolidated sheet; ADR-0059 §7;
-- 03-data-model §3.21). The consolidated asset history sheet translates a
-- member's movements of the year at each period's rate, so it needs the
-- year's sums by period, which class_year_total does not keep. A class's
-- totals of one fiscal period in one ledger of a company: the sums of its
-- movements posted in that period, by the sheet's columns, kept with every
-- posting as class_year_total is (keepTotals), so the sums over a year's
-- periods are that year's class_year_total row. The profit and loss
-- columns (depreciation, impairment losses and their reversals) keep their
-- booked group amounts too (the movements' accumulated_gc), which the
-- consolidated sheet shows as ADR-0059 §7 translates income and expenses.
CREATE TABLE assets.class_period_total (
  tenant_id               uuid NOT NULL,
  legal_entity_id         uuid NOT NULL,      -- tenancy context
  ledger_id               uuid NOT NULL,      -- ledger context
  fiscal_year             smallint NOT NULL,
  fiscal_period           smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  class_code              text NOT NULL,
  acquisitions            bigint NOT NULL DEFAULT 0,
  retired_cost            bigint NOT NULL DEFAULT 0,
  transferred_cost        bigint NOT NULL DEFAULT 0,
  depreciation            bigint NOT NULL DEFAULT 0,
  retired_accumulated     bigint NOT NULL DEFAULT 0,
  transferred_accumulated bigint NOT NULL DEFAULT 0,
  impairment_losses       bigint NOT NULL DEFAULT 0,
  impairment_reversals    bigint NOT NULL DEFAULT 0,
  depreciation_gc         bigint NOT NULL DEFAULT 0,  -- group currency, as booked
  impairment_losses_gc    bigint NOT NULL DEFAULT 0,
  impairment_reversals_gc bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code)
);
SELECT platform.secure_table('assets.class_period_total'::regclass);

-- No version (nothing reads them for an update), so secure_table grants
-- no update: the application may add to the sums alone, never move a row
-- to another key, and deletes nothing.
GRANT UPDATE (acquisitions, retired_cost, transferred_cost, depreciation, retired_accumulated, transferred_accumulated, impairment_losses,
  impairment_reversals, depreciation_gc, impairment_losses_gc, impairment_reversals_gc) ON assets.class_period_total TO erp_app;

-- The totals of the movements written so far, as 000207 backfilled the
-- year totals: the movements locked SHARE first, so a writer of the
-- release before waits until this commits and none is missed while it
-- runs. A replica of the release before that posted after this commits
-- would write movements these totals miss; no release that posts assets
-- has been deployed (PO 2026-11-02: deployment comes last), so none can.
-- Tenant rows: under a role that does not bypass row-level security this
-- fails rather than total nothing, as 000042.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;
LOCK TABLE assets.movement IN SHARE MODE;

INSERT INTO assets.class_period_total (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code, acquisitions, retired_cost,
  transferred_cost, depreciation, retired_accumulated, transferred_accumulated, impairment_losses, impairment_reversals, depreciation_gc,
  impairment_losses_gc, impairment_reversals_gc)
SELECT tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code,
  coalesce(sum(cost) FILTER (WHERE kind = 'acquisition'), 0), coalesce(sum(cost) FILTER (WHERE kind = 'retirement'), 0),
  coalesce(sum(cost) FILTER (WHERE kind IN ('transfer_in', 'transfer_out')), 0), coalesce(sum(accumulated) FILTER (WHERE kind = 'depreciation'), 0),
  coalesce(sum(accumulated) FILTER (WHERE kind = 'retirement'), 0),
  coalesce(sum(accumulated) FILTER (WHERE kind IN ('transfer_in', 'transfer_out')), 0),
  coalesce(sum(accumulated) FILTER (WHERE kind = 'impairment'), 0), coalesce(sum(accumulated) FILTER (WHERE kind = 'impairment_reversal'), 0),
  coalesce(sum(accumulated_gc) FILTER (WHERE kind = 'depreciation'), 0), coalesce(sum(accumulated_gc) FILTER (WHERE kind = 'impairment'), 0),
  coalesce(sum(accumulated_gc) FILTER (WHERE kind = 'impairment_reversal'), 0)
FROM assets.movement
GROUP BY tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, class_code;
