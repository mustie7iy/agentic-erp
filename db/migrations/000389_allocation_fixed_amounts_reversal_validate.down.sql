-- The one-run rule again: a period run again after a reversal goes with
-- the reversals, which 000387's down drops; their entries stay in the
-- journal. Validation only checked the rows; 000387's down puts the
-- previous checks back.
DELETE FROM controlling.allocation_run_reversal;
DELETE FROM controlling.allocation_run WHERE attempt > 1;
ALTER TABLE controlling.allocation_run ADD CONSTRAINT allocation_run_once UNIQUE (tenant_id, cycle_id, fiscal_year, fiscal_period);
