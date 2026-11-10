CREATE OR REPLACE FUNCTION assets.check_asset_terms() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.booked_through_year IS NOT NULL AND (NEW.method, NEW.useful_life_months, NEW.declining_factor, NEW.residual_value, NEW.depreciation_start)
      IS DISTINCT FROM (OLD.method, OLD.useful_life_months, OLD.declining_factor, OLD.residual_value, OLD.depreciation_start) THEN
    RAISE EXCEPTION 'asset %: its terms in ledger % are fixed, depreciation having been posted', OLD.asset_id, OLD.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_in_use';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE assets.asset_terms DROP COLUMN estimate_changes;
DROP TABLE assets.estimate_change;
DROP FUNCTION assets.check_estimate_change();
