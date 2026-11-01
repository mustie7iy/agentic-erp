-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000175's state by adding each one back NOT VALID.
ALTER TABLE ledger.parked_entry
  DROP CONSTRAINT parked_entry_limit_currency_fkey,
  ADD CONSTRAINT parked_entry_limit_currency_fkey FOREIGN KEY (limit_currency) REFERENCES ref.currency NOT VALID,
  DROP CONSTRAINT parked_entry_limit_debits_currency_fkey,
  ADD CONSTRAINT parked_entry_limit_debits_currency_fkey FOREIGN KEY (limit_debits_currency) REFERENCES ref.currency NOT VALID,
  DROP CONSTRAINT parked_entry_limit_by_service,
  ADD CONSTRAINT parked_entry_limit_by_service CHECK (limit_kind IS NULL OR created_by_kind = 'service') NOT VALID,
  DROP CONSTRAINT parked_entry_limit_figures,
  ADD CONSTRAINT parked_entry_limit_figures CHECK ((limit_kind IS NULL) = (limit_debits IS NULL)
      AND (limit_kind IS NULL) = (limit_debits_currency IS NULL) AND (limit_kind IS NULL) = (limit_amount IS NULL)
      AND (limit_kind IS NULL) = (limit_currency IS NULL) AND limit_debits >= 0 AND limit_amount >= 0) NOT VALID,
  DROP CONSTRAINT parked_entry_limit_kind_check,
  ADD CONSTRAINT parked_entry_limit_kind_check CHECK (limit_kind IN ('service_posting_limit')) NOT VALID;
