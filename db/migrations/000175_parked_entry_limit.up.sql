-- Why the ERP parked an entry instead of posting it (ADR-0049 §7, product
-- owner 2026-10-22, T61n): a journal entry a service principal posts
-- through the API whose debits, in its company's currency, exceed the
-- company's service posting limit is parked for a person. The entry keeps
-- the limit and its debits as they were measured, each in its currency, so
-- the person who reviews it reads why; nothing rewrites them.
-- Expand only (06 §6.4): nullable columns without a default rewrite
-- nothing, and the checks and keys are added NOT VALID, reading none of
-- the rows under the lock; 000176 validates them.
ALTER TABLE ledger.parked_entry
  ADD COLUMN limit_kind text,                 -- the limit it was parked by: service_posting_limit
  ADD COLUMN limit_debits bigint,             -- its debits when measured, minor units of limit_debits_currency (the entry's currency then)
  ADD COLUMN limit_debits_currency char(3),
  ADD COLUMN limit_amount bigint,             -- the limit, minor units of limit_currency (its company's)
  ADD COLUMN limit_currency char(3),
  ADD CONSTRAINT parked_entry_limit_kind_check CHECK (limit_kind IN ('service_posting_limit')) NOT VALID,
  ADD CONSTRAINT parked_entry_limit_figures CHECK ((limit_kind IS NULL) = (limit_debits IS NULL)
      AND (limit_kind IS NULL) = (limit_debits_currency IS NULL) AND (limit_kind IS NULL) = (limit_amount IS NULL)
      AND (limit_kind IS NULL) = (limit_currency IS NULL) AND limit_debits >= 0 AND limit_amount >= 0) NOT VALID,
  -- Only a service principal's entry is parked by its limit.
  ADD CONSTRAINT parked_entry_limit_by_service CHECK (limit_kind IS NULL OR created_by_kind = 'service') NOT VALID,
  ADD CONSTRAINT parked_entry_limit_debits_currency_fkey FOREIGN KEY (limit_debits_currency) REFERENCES ref.currency NOT VALID,
  ADD CONSTRAINT parked_entry_limit_currency_fkey FOREIGN KEY (limit_currency) REFERENCES ref.currency NOT VALID;

-- Why an entry was parked is recorded as it is parked and never rewritten,
-- as who parked it is not (check_parked_entry_change).
CREATE FUNCTION ledger.keep_parked_entry_limit() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.limit_kind, NEW.limit_debits, NEW.limit_debits_currency, NEW.limit_amount, NEW.limit_currency)
     IS DISTINCT FROM (OLD.limit_kind, OLD.limit_debits, OLD.limit_debits_currency, OLD.limit_amount, OLD.limit_currency) THEN
    RAISE EXCEPTION 'why parked entry % was parked does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER parked_entry_keep_limit BEFORE UPDATE ON ledger.parked_entry
  FOR EACH ROW EXECUTE FUNCTION ledger.keep_parked_entry_limit();
