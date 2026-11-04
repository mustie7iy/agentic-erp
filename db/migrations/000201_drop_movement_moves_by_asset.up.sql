-- migrate:no-transaction
-- 000200's covering index serves every reader of 000198's (T57f3), so it
-- goes; dropped concurrently (06 §6.4), so no posting waits on it.
DROP INDEX CONCURRENTLY IF EXISTS assets.movement_moves_by_asset;
