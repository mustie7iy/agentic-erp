-- migrate:no-transaction
-- A depreciation run's batches and its preview (T57f2, ADR-0057 §7) walk
-- each asset's schedule from its movements other than depreciation, and
-- read what is booked from the year totals (000197): an asset depreciated
-- for ten years has some 120 depreciation movements and a handful of
-- others, so this index holds the others alone, in the order the walk
-- applies them. Built concurrently (06 §6.4), so no posting waits on the
-- movements while it builds; a failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY movement_moves_by_asset ON assets.movement (tenant_id, asset_id, ledger_id, seq) WHERE kind <> 'depreciation';
