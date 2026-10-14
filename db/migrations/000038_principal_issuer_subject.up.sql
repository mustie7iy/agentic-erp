-- migrate:no-transaction
-- A principal is found by its issuer and subject (F7, ADR-0010): unique per
-- tenant, and the index the unit of work resolves tokens with. Built
-- concurrently (06-engineering-process §6.4). While principal_subject (the
-- subject alone) stays, it is the stricter rule; this index carries
-- uniqueness once the contract step drops that one. A failed build leaves an
-- invalid index that is dropped by hand before the retry, which is why
-- there is no IF NOT EXISTS to pass over it.
CREATE UNIQUE INDEX CONCURRENTLY principal_issuer_subject ON identity.principal (tenant_id, external_issuer, external_subject) WHERE external_subject IS NOT NULL;
