-- Changes in estimate (T57j, ADR-0057 §7; IAS 16.51, 16.61, IAS 8.36-38;
-- the accounting specialist, po:2026-11-09, B13): an asset's useful life,
-- residual value and method in one ledger changed after depreciation was
-- posted there, prospectively from a chosen regular period after the one
-- it is booked through. Each change keeps the terms before and after it,
-- so the schedule walks the earlier periods on the earlier terms and what
-- was planned for them, and booked, stays; the terms row holds the latest
-- change's terms. Insert-only: a later change, from the same period or a
-- later one, supersedes it.
CREATE TABLE assets.estimate_change (
  tenant_id                  uuid NOT NULL,
  id                         uuid NOT NULL,
  asset_id                   uuid NOT NULL,
  ledger_id                  uuid NOT NULL,      -- ledger context
  seq                        integer NOT NULL CHECK (seq >= 1),
  fiscal_year                smallint NOT NULL,
  fiscal_period              smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 12),
  effective_from             date NOT NULL,      -- the period's first day
  method                     text NOT NULL CHECK (method IN ('straight_line', 'declining_balance')),
  useful_life_months         integer NOT NULL CHECK (useful_life_months BETWEEN 1 AND 1200),
  declining_factor           numeric(6,4) CHECK (declining_factor > 1 AND declining_factor <= 10),
  residual_value             bigint NOT NULL CHECK (residual_value >= 0),
  before_method              text NOT NULL CHECK (before_method IN ('straight_line', 'declining_balance')),
  before_useful_life_months  integer NOT NULL CHECK (before_useful_life_months BETWEEN 1 AND 1200),
  before_declining_factor    numeric(6,4) CHECK (before_declining_factor > 1 AND before_declining_factor <= 10),
  before_residual_value      bigint NOT NULL CHECK (before_residual_value >= 0),
  created_at                 timestamptz NOT NULL DEFAULT now(),
  created_by                 uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, asset_id, ledger_id, seq),
  FOREIGN KEY (tenant_id, asset_id, ledger_id) REFERENCES assets.asset_terms (tenant_id, asset_id, ledger_id),
  CONSTRAINT estimate_change_declining_factor CHECK ((method = 'declining_balance') = (declining_factor IS NOT NULL)
    AND (before_method = 'declining_balance') = (before_declining_factor IS NOT NULL))
);
CREATE TRIGGER estimate_change_insert_only BEFORE UPDATE OR DELETE ON assets.estimate_change
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('assets.estimate_change'::regclass);

-- The number of changes in estimate of the terms: the walk reads the
-- changes only of terms that have some, so a run over assets without any
-- issues no statement more.
ALTER TABLE assets.asset_terms ADD COLUMN estimate_changes integer NOT NULL DEFAULT 0 CHECK (estimate_changes >= 0);

-- A change in estimate is prospective: from a period after the one its
-- ledger is booked through (depreciation having been posted there), on
-- or after the latest change's period, taking the asset's current seq
-- (its writer raised last_seq under the asset's lock), its terms before
-- being the terms row's as they stand.
CREATE FUNCTION assets.check_estimate_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE t assets.asset_terms; current integer; latest assets.estimate_change;
BEGIN
  SELECT * INTO t FROM assets.asset_terms WHERE tenant_id = NEW.tenant_id AND asset_id = NEW.asset_id AND ledger_id = NEW.ledger_id;
  IF t.booked_through_year IS NULL OR (NEW.fiscal_year, NEW.fiscal_period) <= (t.booked_through_year, t.booked_through_period) THEN
    RAISE EXCEPTION 'asset %: a change in estimate in ledger % is from a period after the one depreciation is booked through', NEW.asset_id, NEW.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'estimate_change_prospective';
  END IF;
  SELECT * INTO latest FROM assets.estimate_change WHERE tenant_id = NEW.tenant_id AND asset_id = NEW.asset_id AND ledger_id = NEW.ledger_id
    ORDER BY seq DESC LIMIT 1;
  IF latest.id IS NOT NULL AND (NEW.effective_from < latest.effective_from OR NEW.seq <= latest.seq) THEN
    RAISE EXCEPTION 'asset %: a change in estimate in ledger % follows the latest one, from % on', NEW.asset_id, NEW.ledger_id, latest.effective_from
      USING ERRCODE = 'check_violation', CONSTRAINT = 'estimate_change_prospective';
  END IF;
  SELECT last_seq INTO current FROM assets.asset WHERE tenant_id = NEW.tenant_id AND id = NEW.asset_id;
  IF NEW.seq <> current THEN
    RAISE EXCEPTION 'asset %: a change in estimate takes the asset''s seq %, not %', NEW.asset_id, current, NEW.seq
      USING ERRCODE = 'check_violation', CONSTRAINT = 'estimate_change_seq';
  END IF;
  IF (NEW.before_method, NEW.before_useful_life_months, NEW.before_declining_factor, NEW.before_residual_value)
      IS DISTINCT FROM (t.method, t.useful_life_months, t.declining_factor, t.residual_value) THEN
    RAISE EXCEPTION 'asset %: a change in estimate in ledger % records the terms as they stand', NEW.asset_id, NEW.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'estimate_change_before';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_estimate_change() OWNER TO erp_owner;
CREATE TRIGGER estimate_change_prospective BEFORE INSERT ON assets.estimate_change FOR EACH ROW EXECUTE FUNCTION assets.check_estimate_change();

-- Terms are fixed once depreciation is posted in their ledger (decided by
-- default 8), except by a change in estimate (T57j): the update that
-- counts one more change may set the method, useful life, factor and
-- residual value to the latest change's, and its terms must be that
-- change's; the depreciation start never changes then. The count only
-- grows, one at a time.
CREATE OR REPLACE FUNCTION assets.check_asset_terms() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE c assets.estimate_change;
BEGIN
  IF NEW.estimate_changes NOT IN (OLD.estimate_changes, OLD.estimate_changes + 1) THEN
    RAISE EXCEPTION 'asset %: its changes in estimate in ledger % are counted one at a time', OLD.asset_id, OLD.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_in_use';
  END IF;
  IF NEW.estimate_changes = OLD.estimate_changes + 1 THEN
    SELECT * INTO c FROM assets.estimate_change WHERE tenant_id = NEW.tenant_id AND asset_id = NEW.asset_id AND ledger_id = NEW.ledger_id
      ORDER BY seq DESC LIMIT 1;
    IF c.id IS NULL OR (NEW.method, NEW.useful_life_months, NEW.declining_factor, NEW.residual_value, NEW.depreciation_start)
        IS DISTINCT FROM (c.method, c.useful_life_months, c.declining_factor, c.residual_value, OLD.depreciation_start) THEN
      RAISE EXCEPTION 'asset %: its terms in ledger % take the latest change in estimate''s', OLD.asset_id, OLD.ledger_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_in_use';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.booked_through_year IS NOT NULL AND (NEW.method, NEW.useful_life_months, NEW.declining_factor, NEW.residual_value, NEW.depreciation_start)
      IS DISTINCT FROM (OLD.method, OLD.useful_life_months, OLD.declining_factor, OLD.residual_value, OLD.depreciation_start) THEN
    RAISE EXCEPTION 'asset %: its terms in ledger % are fixed, depreciation having been posted', OLD.asset_id, OLD.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_in_use';
  END IF;
  RETURN NEW;
END $$;
