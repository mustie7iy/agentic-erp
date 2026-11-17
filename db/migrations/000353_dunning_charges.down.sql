ALTER TABLE collections.dunning_notice_item DROP COLUMN interest_amount;
ALTER TABLE collections.dunning_notice DROP COLUMN charge_amount, DROP COLUMN interest_amount;
ALTER TABLE collections.dunning_run DROP CONSTRAINT dunning_run_below_minimum_check, DROP COLUMN grace_days, DROP COLUMN minimum_amount,
  DROP COLUMN interest_rate_percent, DROP COLUMN charge_amount, DROP COLUMN interest_amount, DROP COLUMN below_minimum_count,
  DROP COLUMN below_minimum_amount;
