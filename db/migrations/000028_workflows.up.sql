-- Temporal runtime (T26, ADR-0012 accepted): an operation whose work runs as
-- a workflow records the workflow id the dispatcher started it under, so a
-- cancellation reaches the workflow and a restart starts nothing twice. The
-- dispatcher finds queued operations across tenants as the worker role and
-- derives the workflow id from the run's business key.
ALTER TABLE platform.operation ADD COLUMN workflow_id text;
GRANT SELECT (workflow_id, version), UPDATE (workflow_id, version) ON platform.operation TO erp_worker;
GRANT SELECT (id, legal_entity_id, document_number) ON payments.payment_run TO erp_worker;
