REVOKE SELECT (tenant_id, id, status, workflow_id, version), UPDATE (workflow_id, version) ON workflow.approval_request FROM erp_worker;
REVOKE USAGE ON SCHEMA workflow FROM erp_worker;
ALTER TABLE workflow.approval_request DROP COLUMN workflow_id;
ALTER TABLE workflow.approval_request DROP COLUMN escalated_at;
ALTER TABLE workflow.approval_request DROP COLUMN escalations;
