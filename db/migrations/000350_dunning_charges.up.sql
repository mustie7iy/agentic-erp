-- Dunning charges, interest, grace days and minimum amounts (T76d;
-- ADR-0065 As built; 03-data-model §3.27): a run keeps the company's
-- dunning procedure as it applied it (grace days, minimum amount, annual
-- interest rate), the charges and interest it proposes and the notices it
-- left out for being below the minimum amount; a notice its charge and
-- interest, an item its interest. Amounts are minor units of the run's
-- currency, the company's. Nothing is posted: the charges and interest
-- are what the notices ask for.
-- Expand only (06 §6.4): columns with defaults, which the previous
-- version neither reads nor writes; its runs charge nothing, give no
-- grace and have no minimum. The application's update grant on a run
-- (status, version, updated_by) leaves them out, and notices and items
-- stay insert-only.
ALTER TABLE collections.dunning_run
  ADD COLUMN grace_days smallint NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_grace_days_check CHECK (grace_days BETWEEN 0 AND 365),
  ADD COLUMN minimum_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_minimum_amount_check CHECK (minimum_amount >= 0),
  ADD COLUMN interest_rate_percent numeric(6,3)
    CONSTRAINT dunning_run_interest_rate_check CHECK (interest_rate_percent > 0 AND interest_rate_percent < 100),
  ADD COLUMN charge_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_charge_amount_check CHECK (charge_amount >= 0),
  ADD COLUMN interest_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_interest_amount_check CHECK (interest_amount >= 0),
  ADD COLUMN below_minimum_count integer NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_below_minimum_count_check CHECK (below_minimum_count >= 0),
  ADD COLUMN below_minimum_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_run_below_minimum_amount_check CHECK (below_minimum_amount >= 0),
  -- Notices left out for being below the minimum have an amount, and are
  -- below a minimum.
  ADD CONSTRAINT dunning_run_below_minimum_check CHECK ((below_minimum_count = 0) = (below_minimum_amount = 0)
    AND (below_minimum_count = 0 OR minimum_amount > 0));

ALTER TABLE collections.dunning_notice
  ADD COLUMN charge_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_notice_charge_amount_check CHECK (charge_amount >= 0),
  ADD COLUMN interest_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_notice_interest_amount_check CHECK (interest_amount >= 0);

ALTER TABLE collections.dunning_notice_item
  ADD COLUMN interest_amount bigint NOT NULL DEFAULT 0
    CONSTRAINT dunning_notice_item_interest_amount_check CHECK (interest_amount >= 0);
