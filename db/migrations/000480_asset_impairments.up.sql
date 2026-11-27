-- Impairment losses recognised and reversed on an asset, per ledger
-- (po:2026-11-09, B14; ADR-0057 As built, impairment; IAS 36.59-63,
-- 36.114-119; 03-data-model §3.21). A person gives the amount: the loss
-- (kind loss) or the reversal of earlier losses (kind reversal), in the
-- company's local currency, valued in its posting period, posted to one
-- ledger. It reduces or restores the asset's carrying amount at the end
-- of the period of its value date, after that period's depreciation, so
-- every later period depreciates the carrying amount then over the life
-- left (IAS 36.63). Each one writes one movement of the asset in its
-- ledger, kind impairment or impairment_reversal, which changes the
-- accumulated depreciation alone (aggregated with accumulated impairment,
-- IAS 16.73(d)); the year totals the history sheet reads take them in
-- columns of their own. The document keeps the carrying amount it was
-- measured against and the one the asset would have had without any
-- impairment (IAS 36.117's ceiling of a reversal). Insert-only: an
-- impairment is not reversed as a document; a reversal of the loss is a
-- document of its own.
-- Expand only: the checks naming the kinds are widened, and the new
-- checks on the movements added, NOT VALID (validated by 000481); the
-- previous version writes none of the new kinds, and the new columns of
-- the year totals default to zero.
ALTER TABLE assets.movement
  DROP CONSTRAINT movement_kind_check,
  ADD CONSTRAINT movement_kind_check
    CHECK (kind IN ('acquisition', 'depreciation', 'retirement', 'transfer_out', 'transfer_in', 'impairment', 'impairment_reversal')) NOT VALID,
  DROP CONSTRAINT movement_document_kind_check,
  ADD CONSTRAINT movement_document_kind_check
    CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'depreciation_run', 'impairment')) NOT VALID,
  -- An impairment or its reversal changes the accumulated depreciation
  -- alone, a loss as a credit (negative), a reversal as a debit
  -- (positive); a reversal row of either, negating it, the other sign.
  ADD CONSTRAINT movement_impairment CHECK (kind NOT IN ('impairment', 'impairment_reversal')
    OR (cost = 0 AND cost_gc = 0 AND residual = 0 AND proceeds = 0 AND share IS NULL AND document_kind = 'impairment'
      AND (reverses_id IS NOT NULL OR (kind = 'impairment') = (accumulated < 0)) AND accumulated <> 0)) NOT VALID,
  ADD CONSTRAINT movement_impairment_document CHECK (document_kind <> 'impairment' OR kind IN ('impairment', 'impairment_reversal')) NOT VALID;

ALTER TABLE assets.document_entry
  DROP CONSTRAINT document_entry_document_kind_check,
  ADD CONSTRAINT document_entry_document_kind_check CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'impairment')) NOT VALID;

-- The history sheet's impairment lines (IAS 16.73(e)(v), (vi)): the
-- impairment movements' accumulated (losses, negative) and the
-- impairment_reversal movements' (reversals, positive), by posting year.
ALTER TABLE assets.year_total
  ADD COLUMN impairment_losses    bigint NOT NULL DEFAULT 0,
  ADD COLUMN impairment_reversals bigint NOT NULL DEFAULT 0;
ALTER TABLE assets.class_year_total
  ADD COLUMN impairment_losses    bigint NOT NULL DEFAULT 0,
  ADD COLUMN impairment_reversals bigint NOT NULL DEFAULT 0;
GRANT UPDATE (impairment_losses, impairment_reversals) ON assets.year_total, assets.class_year_total TO erp_app;

-- An impairment loss or its reversal on an asset in one ledger: the
-- amount (positive, in the company's local currency) and its group
-- amount at the posting date's rate; the carrying amount at the end of
-- the value date's period it was measured against, and the carrying
-- amount the asset would have had there without any impairment, both as
-- the schedule plans them, before this document.
CREATE TABLE assets.impairment (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,      -- tenancy context
  asset_id            uuid NOT NULL,
  seq                 integer NOT NULL CHECK (seq >= 1),
  ledger_id           uuid NOT NULL,      -- ledger context
  kind                text NOT NULL CHECK (kind IN ('loss', 'reversal')),
  value_date          date NOT NULL,
  posting_date        date NOT NULL,
  fiscal_year         smallint NOT NULL,  -- the posting date's
  fiscal_period       smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 12),
  currency            char(3) NOT NULL REFERENCES ref.currency,  -- the company's local currency
  amount              bigint NOT NULL CHECK (amount > 0),
  currency_gc         char(3) NOT NULL REFERENCES ref.currency,  -- the tenant's group currency
  amount_gc           bigint NOT NULL,
  carrying_amount     bigint NOT NULL,
  unimpaired_amount   bigint NOT NULL,
  text                text CHECK (length(text) BETWEEN 1 AND 200),
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id),
  CONSTRAINT impairment_seq_once UNIQUE (tenant_id, asset_id, seq),
  -- Not backdated, as a retirement (decided by default 13): valued in its
  -- posting period, on or before its posting date.
  CONSTRAINT impairment_value_date CHECK (value_date <= posting_date
    AND date_trunc('month', value_date::timestamp) = date_trunc('month', posting_date::timestamp)),
  -- A loss takes at most the carrying amount, and a reversal restores at
  -- most what the impairments took, net of the depreciation they saved
  -- (IAS 36.117).
  CONSTRAINT impairment_limit CHECK (CASE kind WHEN 'loss' THEN amount <= carrying_amount ELSE amount <= unimpaired_amount - carrying_amount END)
);
CREATE TRIGGER impairment_insert_only BEFORE UPDATE OR DELETE ON assets.impairment FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE INDEX impairment_by_company ON assets.impairment (tenant_id, legal_entity_id, posting_date, id);
CREATE INDEX impairment_by_asset ON assets.impairment (tenant_id, asset_id, seq);
SELECT platform.secure_table('assets.impairment'::regclass);
