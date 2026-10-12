REVOKE SELECT (tenant_id, status, valid_to) ON procurement.contract FROM erp_worker;
REVOKE USAGE ON SCHEMA procurement FROM erp_worker;
