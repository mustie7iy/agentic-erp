REVOKE SELECT (tenant_id, posted_xid, posted_at) ON ledger.journal_entry FROM erp_worker;
REVOKE USAGE ON SCHEMA ledger FROM erp_worker;
REVOKE UPDATE (claimed_lsn) ON analytics.extract_due FROM erp_worker;
ALTER TABLE analytics.extract_due DROP COLUMN claimed_lsn;
REVOKE INSERT (tenant_id, changed_at), UPDATE (changed_at) ON analytics.extract_due FROM erp_worker;
REVOKE SELECT, DELETE ON analytics.snapshot FROM erp_worker;
DROP TABLE analytics.superseded;
