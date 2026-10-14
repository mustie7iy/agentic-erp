-- A principal is the subject of one identity provider, not of any issuer the
-- cell trusts (F7, ADR-0010 amendment of 2026-10-14): every trusted issuer
-- may name any tenant, so matching on the subject alone let one tenant's
-- identity provider sign in as another tenant's principal whose subject it
-- also issues. The unit of work now matches issuer and subject.
--
-- Expand step (06-engineering-process §6.4): the column is added nullable,
-- so the previous release, which inserts principals without it, keeps
-- running during the rollout; the unique index on (tenant_id,
-- external_issuer, external_subject) is built concurrently by 000038. The
-- contract step, a later release, adds the check that people and services
-- carry an issuer and drops principal_subject (the subject alone unique per
-- tenant), which the previous release still relies on.
--
-- Backfill: no record says which issuer a principal was created under (the
-- tenant row, the audit trail and the events name none). Existing
-- principals get the development issuer, erp-dev: the installations that
-- hold principals today are developers' ones, whose single trusted issuer it
-- is, so their logins keep working. Anywhere else it matches no token, since
-- ERP_ENV=production refuses the development issuer, and a principal created
-- before this migration is refused (identity.principal_unknown) until it is
-- bound to its real issuer: fail closed rather than guess. A fresh install
-- has no principals to backfill. One statement suffices: there is no
-- production data yet; a large table would be backfilled in batches.
ALTER TABLE identity.principal ADD COLUMN external_issuer text;
COMMENT ON COLUMN identity.principal.external_issuer IS
  'The OIDC issuer (iss) whose tokens authenticate the principal, with external_subject; NULL for agents';
UPDATE identity.principal SET external_issuer = 'erp-dev', version = version + 1
  WHERE external_subject IS NOT NULL AND external_issuer IS NULL;
