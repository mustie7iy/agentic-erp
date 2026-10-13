-- Approval deadlines (T29, ADR-0012): a pending approval request is watched by
-- a workflow that escalates it when no decision arrives within the deadline.
-- The request records how often it was escalated and when last; the
-- dispatcher finds pending requests without a workflow across tenants as the
-- worker role.
ALTER TABLE workflow.approval_request ADD COLUMN escalations smallint NOT NULL DEFAULT 0 CHECK (escalations >= 0);
ALTER TABLE workflow.approval_request ADD COLUMN escalated_at timestamptz;
ALTER TABLE workflow.approval_request ADD COLUMN workflow_id text;
GRANT USAGE ON SCHEMA workflow TO erp_worker;
GRANT SELECT (tenant_id, id, status, workflow_id, version), UPDATE (workflow_id, version) ON workflow.approval_request TO erp_worker;
