-- migrate:no-transaction
-- Built again concurrently; it fails, leaving an invalid index to drop by
-- hand, if two principals now share a subject under different issuers.
CREATE UNIQUE INDEX CONCURRENTLY principal_subject ON identity.principal (tenant_id, external_subject) WHERE external_subject IS NOT NULL;
