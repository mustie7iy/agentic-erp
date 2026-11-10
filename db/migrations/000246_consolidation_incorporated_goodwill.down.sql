-- NOT VALID: an incorporated member's rows written since need not hold
-- the residual; the rule binds the rows written after the rollback.
ALTER TABLE consolidation.acquisition DROP CONSTRAINT acquisition_goodwill,
  ADD CONSTRAINT acquisition_goodwill CHECK (goodwill_gc = investment_gc - net_assets_gc + nci_gc) NOT VALID;
ALTER TABLE consolidation.run_acquisition DROP CONSTRAINT run_acquisition_goodwill,
  ADD CONSTRAINT run_acquisition_goodwill CHECK (goodwill_gc = investment_gc - net_assets_gc + nci_gc) NOT VALID;
