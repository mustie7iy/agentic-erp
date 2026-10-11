REVOKE SELECT (tenant_id, seq) ON audit.anchor FROM erp_worker;
REVOKE SELECT (tenant_id, seq, hash) ON audit.chain_head FROM erp_worker;
REVOKE USAGE ON SCHEMA audit FROM erp_worker;
