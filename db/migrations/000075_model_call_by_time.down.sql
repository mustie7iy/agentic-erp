-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS agents.model_call_by_time;
