-- migrate:no-transaction
-- The moves a depreciation run's batches, its preview, a catch-up and an
-- impairment walk (T57f3, ADR-0057 §7) now need their kind: an impairment
-- or its reversal is applied at the end of its period, after that
-- period's depreciation, every other move at its start (As built,
-- impairment). 000212's covering index lacks the kind, so reading it
-- would fetch every move from the table again; this one holds it too, so
-- they are still read from the index alone. 000472 drops 000212's. Built
-- concurrently (06 §6.4), so no posting waits on the movements while it
-- builds; a failed build leaves an invalid index, dropped by hand before
-- the retry.
CREATE INDEX CONCURRENTLY movement_moves_with_kind ON assets.movement (tenant_id, asset_id, ledger_id, seq)
  INCLUDE (value_date, cost, accumulated, residual, kind) WHERE kind <> 'depreciation';
