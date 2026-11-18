-- Validates what 000375 added NOT VALID (06 §6.4), and drops the one-run
-- rule allocation_run_attempt_once (000376) replaces: a cycle's period
-- may be run again once its run is reversed.
ALTER TABLE controlling.allocation_cycle VALIDATE CONSTRAINT allocation_cycle_share_basis;
ALTER TABLE controlling.allocation_cycle VALIDATE CONSTRAINT allocation_cycle_fixed_amounts;
ALTER TABLE controlling.allocation_receiver VALIDATE CONSTRAINT allocation_receiver_amount;
ALTER TABLE controlling.allocation_run VALIDATE CONSTRAINT allocation_run_account_count;
ALTER TABLE controlling.allocation_run DROP CONSTRAINT allocation_run_once;
