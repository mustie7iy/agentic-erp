-- Audit anchoring (T12, ADR-0011): a worker writes each tenant's chain head
-- to write-once object storage and records it in audit.anchor. Finding the
-- tenants whose head moved on crosses tenants, which the worker role does;
-- writing the anchor runs in the tenant's scope as erp_app.
GRANT USAGE ON SCHEMA audit TO erp_worker;
GRANT SELECT (tenant_id, seq, hash) ON audit.chain_head TO erp_worker;
GRANT SELECT (tenant_id, seq) ON audit.anchor TO erp_worker;
