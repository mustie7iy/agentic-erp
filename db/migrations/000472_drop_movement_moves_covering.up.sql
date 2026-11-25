-- migrate:no-transaction
-- 000471's index serves every reader of 000212's, so it goes; dropped
-- concurrently (06 §6.4), so no posting waits on it.
DROP INDEX CONCURRENTLY IF EXISTS assets.movement_moves_covering;
