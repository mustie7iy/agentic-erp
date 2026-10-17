-- Certificate-bound agent tokens (T38d, ADR-0033, ADR-0031 as amended,
-- 03-data-model §3.4): a tenant's identity provider says whether it binds
-- the tokens it issues to the client's certificate (RFC 8705,
-- cnf.x5t#S256). In production an agent's token is accepted only from a
-- provider that does, and only with the certificate it is bound to.
-- Existing registrations bind nothing until an operator says they do.
ALTER TABLE identity.identity_provider ADD COLUMN certificate_bound boolean NOT NULL DEFAULT false;
GRANT SELECT (certificate_bound) ON identity.identity_provider TO erp_control;
