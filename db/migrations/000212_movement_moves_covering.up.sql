-- migrate:no-transaction
-- A depreciation run's batches, its preview and a catch-up (T57f3,
-- ADR-0057 §7) read each asset's movements other than depreciation, the
-- moves its schedule walks, and only their value date, cost, accumulated
-- depreciation and residual value. 000208's index finds them, but each
-- is then read from the table, where an asset's few such movements lie
-- among its depreciation (the preview of 100,000 assets read 90,000
-- pages, 0.45 s); this one holds the columns, so they are read from the
-- index alone (an index-only scan, the movements being insert-only and
-- vacuumed). 000213 drops 000208's. Built concurrently (06 §6.4), so no
-- posting waits on the movements while it builds; a failed build leaves
-- an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY movement_moves_covering ON assets.movement (tenant_id, asset_id, ledger_id, seq)
  INCLUDE (value_date, cost, accumulated, residual) WHERE kind <> 'depreciation';
