-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS controlling.allocation_run_attempt_once;
