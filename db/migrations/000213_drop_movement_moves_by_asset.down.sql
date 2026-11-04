-- migrate:no-transaction
CREATE INDEX CONCURRENTLY movement_moves_by_asset ON assets.movement (tenant_id, asset_id, ledger_id, seq) WHERE kind <> 'depreciation';
