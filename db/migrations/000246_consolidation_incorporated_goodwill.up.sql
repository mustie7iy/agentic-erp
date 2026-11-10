-- An incorporated member has no goodwill (T68h2; ADR-0059 As built T68h2;
-- the accounting specialist's B5(a), po:2026-11-09): IFRS 3 does not
-- apply to a subsidiary the parent incorporated, and its investment is
-- eliminated against its share capital alone, so its retained earnings
-- and translation reserve stay in the group and its investment is not its
-- net assets at the period before its first. Goodwill stays the residual
-- (investment - net assets + the interest, §9) for an acquired member.
-- Expand only: the previous version wrote an incorporated member's
-- investment equal to its net assets, so its rows hold either rule.
ALTER TABLE consolidation.run_acquisition DROP CONSTRAINT run_acquisition_goodwill,
  ADD CONSTRAINT run_acquisition_goodwill CHECK (CASE origin WHEN 'incorporated' THEN goodwill_gc = 0
    ELSE goodwill_gc = investment_gc - net_assets_gc + nci_gc END);
ALTER TABLE consolidation.acquisition DROP CONSTRAINT acquisition_goodwill,
  ADD CONSTRAINT acquisition_goodwill CHECK (CASE origin WHEN 'incorporated' THEN goodwill_gc = 0
    ELSE goodwill_gc = investment_gc - net_assets_gc + nci_gc END);
