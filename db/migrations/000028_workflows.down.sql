REVOKE SELECT (id, legal_entity_id, document_number) ON payments.payment_run FROM erp_worker;
REVOKE SELECT (workflow_id, version), UPDATE (workflow_id, version) ON platform.operation FROM erp_worker;
ALTER TABLE platform.operation DROP COLUMN workflow_id;
