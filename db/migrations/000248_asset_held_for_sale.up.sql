-- Depreciation stopped at classification as held for sale (T57k, ADR-0057
-- §7; IAS 16.55, IFRS 5.25; the accounting specialist, po:2026-11-09,
-- B11): per ledger, the regular period from which runs no longer
-- depreciate the asset (the first day of that period in
-- held_for_sale_from). Set from a period after the one the ledger is
-- booked through, and moved only while neither period is booked, so no
-- depreciation booked changes; it is not cleared (a reclassification out
-- of held for sale, IFRS 5.26-29, is not built).
ALTER TABLE assets.asset_terms
  ADD COLUMN held_for_sale_year smallint,
  ADD COLUMN held_for_sale_period smallint CHECK (held_for_sale_period BETWEEN 1 AND 12),
  ADD COLUMN held_for_sale_from date,
  ADD CONSTRAINT asset_terms_held_for_sale CHECK ((held_for_sale_year IS NULL) = (held_for_sale_period IS NULL)
    AND (held_for_sale_year IS NULL) = (held_for_sale_from IS NULL));

CREATE OR REPLACE FUNCTION assets.check_asset_terms() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE c assets.estimate_change;
BEGIN
  IF (NEW.held_for_sale_year, NEW.held_for_sale_period, NEW.held_for_sale_from)
      IS DISTINCT FROM (OLD.held_for_sale_year, OLD.held_for_sale_period, OLD.held_for_sale_from)
    AND (NEW.held_for_sale_year IS NULL
      OR (OLD.booked_through_year IS NOT NULL AND ((NEW.held_for_sale_year, NEW.held_for_sale_period) <= (OLD.booked_through_year, OLD.booked_through_period)
        OR (OLD.held_for_sale_year, OLD.held_for_sale_period) <= (OLD.booked_through_year, OLD.booked_through_period)))) THEN
    RAISE EXCEPTION 'asset %: its classification as held for sale in ledger % is from a period after the one depreciation is booked through, and stays',
      OLD.asset_id, OLD.ledger_id USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_terms_held_for_sale_prospective';
  END IF;
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
