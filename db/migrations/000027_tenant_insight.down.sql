REVOKE SELECT (tenant_id, id, master_key_id, status, created_at) ON tenancy.tenant_key FROM erp_control;
REVOKE SELECT (tenant_id, verified_at, head_seq, checked, anchors_checked, broken_at, ok) ON audit.verification FROM erp_control;
REVOKE USAGE ON SCHEMA audit FROM erp_control;
