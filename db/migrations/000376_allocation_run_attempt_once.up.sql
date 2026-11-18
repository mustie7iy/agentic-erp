-- migrate:no-transaction
-- One run of a cycle per period and attempt (T60k; ADR-0064 As built
-- T60k): with the trigger of 000375, one run in force per cycle and
-- period, while a reversed run's period may be run again. Built
-- concurrently in its own file (06 §6.4).
CREATE UNIQUE INDEX CONCURRENTLY allocation_run_attempt_once ON controlling.allocation_run (tenant_id, cycle_id, fiscal_year, fiscal_period, attempt);
