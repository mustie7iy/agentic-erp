-- Contract step of issuer-bound principals, part one (F7, ADR-0010,
-- 03-data-model §3.4, 06-engineering-process §6.4, T31). 000037 bound the
-- principals that existed then; the previous release kept inserting people
-- and services with a subject and no issuer during the rollout, and the
-- new release never finds those (it resolves issuer and subject).
--
-- (1) A principal with a subject must have an issuer. The check is added
-- NOT VALID, so no new row lacks one from here on; 000047 validates it once
-- (2) has bound the rest. The ALTER holds the table's ACCESS EXCLUSIVE lock
-- until this commits, so the backfill below runs under it: no insert of
-- the previous release slips in between the backfill and the check. The
-- table is small (people, services and agents) and has no production data
-- yet; a large one would be bound in batches by an operation first.
ALTER TABLE identity.principal ADD CONSTRAINT principal_issuer_with_subject
  CHECK (external_subject IS NULL OR external_issuer IS NOT NULL) NOT VALID;

-- (2) Each principal still unbound is bound to the issuer of the principal
-- that created it, as CreatePrincipal binds a new one, following chains of
-- unbound creators up to a bound one. Where no bound creator is found, it
-- is bound to the development issuer erp-dev, as 000037 did: a migration
-- cannot read which issuers the cell trusts, and ERP_ENV=production refuses
-- erp-dev, so outside development such a principal is refused
-- (identity.principal_unknown) until an operator binds it to its real
-- issuer (T37): fail closed rather than guess. Row-level security is off
-- for the transaction, so every tenant's principals are bound or, run by a
-- role that does not bypass it, the migration fails. The backfill writes
-- no audit entries: migrations are recorded by the migration table.
SET LOCAL row_security = off;
WITH RECURSIVE bound (tenant_id, id, issuer) AS (
  SELECT tenant_id, id, external_issuer FROM identity.principal WHERE external_issuer IS NOT NULL
  UNION ALL
  SELECT p.tenant_id, p.id, b.issuer
  FROM identity.principal p JOIN bound b ON b.tenant_id = p.tenant_id AND b.id = p.created_by
  WHERE p.external_issuer IS NULL AND p.external_subject IS NOT NULL
)
UPDATE identity.principal p
SET external_issuer = coalesce((SELECT b.issuer FROM bound b WHERE b.tenant_id = p.tenant_id AND b.id = p.id), 'erp-dev'),
    version = version + 1
WHERE p.external_issuer IS NULL AND p.external_subject IS NOT NULL;
