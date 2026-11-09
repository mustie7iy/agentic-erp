-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000225's state by adding the check back NOT VALID.
ALTER TABLE ledger.parked_entry
  DROP CONSTRAINT parked_entry_fiscal_period_special,
  ADD CONSTRAINT parked_entry_fiscal_period_special CHECK (fiscal_period BETWEEN 13 AND 16) NOT VALID;
