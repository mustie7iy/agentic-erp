-- migrate:no-transaction
CREATE INDEX CONCURRENTLY movement_moves_covering ON assets.movement (tenant_id, asset_id, ledger_id, seq)
  INCLUDE (value_date, cost, accumulated, residual) WHERE kind <> 'depreciation';
