DELETE FROM identity.sod_rule WHERE permission_a = 'cash.snapshot.take' OR permission_b = 'cash.snapshot.take';
DELETE FROM identity.role_permission WHERE permission_code = 'cash.snapshot.take';
DELETE FROM ref.permission WHERE code = 'cash.snapshot.take';
-- No earlier migration grants erp_worker anything in the cash schema, so
-- revoking it all restores what was.
REVOKE SELECT (tenant_id, is_active) ON cash.liquidity_item FROM erp_worker;
REVOKE USAGE ON SCHEMA cash FROM erp_worker;
DROP FUNCTION cash.delete_expired_snapshots(uuid, date);
DROP TABLE cash.snapshot_flow;
DROP TABLE cash.snapshot;
