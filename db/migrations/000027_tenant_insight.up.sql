-- The control plane shows a tenant's latest audit verification (ADR-0025)
-- and the key that seals its fields (ADR-0018, ADR-0019) on the tenant
-- itself. The control role reads across tenants; it sees the verification
-- outcome and the key's identifiers, never the wrapped key material.
GRANT USAGE ON SCHEMA audit TO erp_control;
GRANT SELECT (tenant_id, verified_at, head_seq, checked, anchors_checked, broken_at, ok) ON audit.verification TO erp_control;
GRANT SELECT (tenant_id, id, master_key_id, status, created_at) ON tenancy.tenant_key TO erp_control;
