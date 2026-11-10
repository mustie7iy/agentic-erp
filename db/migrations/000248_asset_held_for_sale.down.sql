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
ALTER TABLE assets.asset_terms DROP CONSTRAINT asset_terms_held_for_sale,
  DROP COLUMN held_for_sale_year, DROP COLUMN held_for_sale_period, DROP COLUMN held_for_sale_from;
