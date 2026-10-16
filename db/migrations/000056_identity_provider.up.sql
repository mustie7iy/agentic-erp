-- Per-tenant identity providers and authentication policy (T37, ADR-0031,
-- 03-data-model §3.4). A token's tenant is decided by the active
-- registration of its issuer and audience; the tenant's policy says whether
-- people must prove multi-factor authentication and how long their
-- sessions last. Like the rest of the identity context, neither table has a
-- foreign key into tenancy (02-architecture).
CREATE TABLE identity.identity_provider (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  issuer        text NOT NULL CHECK (length(issuer) <= 512 AND issuer ~ '^https?://[^\s/?#]+(/[^\s?#]*)?$'),
  audience      text NOT NULL CHECK (length(audience) BETWEEN 1 AND 255),
  tenant_claim  text CHECK (tenant_claim ~ '^[A-Za-z_][A-Za-z0-9_.:/-]{0,63}$'),
  acr_values    text[] NOT NULL DEFAULT '{}' CHECK (cardinality(acr_values) <= 16),
  amr_values    text[] NOT NULL DEFAULT '{}' CHECK (cardinality(amr_values) <= 16),
  status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  retired_at    timestamptz,
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT identity_provider_retired_at CHECK ((status = 'retired') = (retired_at IS NOT NULL))
);
-- The pair decides the tenant, so it is unique across tenants: the one key
-- in a tenant schema not led by tenant_id (ADR-0031).
CREATE UNIQUE INDEX identity_provider_issuer_audience ON identity.identity_provider (issuer, audience) WHERE status = 'active';
CREATE INDEX identity_provider_tenant ON identity.identity_provider (tenant_id, issuer) WHERE status = 'active';
CREATE TRIGGER identity_provider_touch BEFORE UPDATE ON identity.identity_provider FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Issuer and audience are the registration's identity: a change is a new
-- registration. A retired registration stays retired.
CREATE FUNCTION identity.check_identity_provider_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'retired' THEN
    RAISE EXCEPTION 'identity provider % is retired', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.issuer <> OLD.issuer OR NEW.audience <> OLD.audience THEN
    RAISE EXCEPTION 'identity provider %: issuer and audience cannot change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER identity_provider_update BEFORE UPDATE ON identity.identity_provider
  FOR EACH ROW EXECUTE FUNCTION identity.check_identity_provider_update();
SELECT platform.secure_table('identity.identity_provider'::regclass);

-- A tenant without a row has the defaults below.
CREATE TABLE identity.auth_policy (
  tenant_id        uuid PRIMARY KEY,
  mfa_required     boolean NOT NULL DEFAULT false,
  session_max_age  interval NOT NULL DEFAULT '12 hours' CHECK (session_max_age BETWEEN '15 minutes' AND '24 hours'),
  idle_timeout     interval NOT NULL DEFAULT '30 minutes' CHECK (idle_timeout BETWEEN '5 minutes' AND '8 hours'),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid,            -- NULL: the platform's defaults, never changed
  version          integer NOT NULL DEFAULT 1,
  CONSTRAINT auth_policy_idle_within_age CHECK (idle_timeout <= session_max_age)
);
CREATE TRIGGER auth_policy_touch BEFORE UPDATE ON identity.auth_policy FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('identity.auth_policy'::regclass);

-- The cell resolves a token's tenant before it knows the tenant: a
-- read-only lookup across tenants as the control role, which also lists a
-- tenant's unbound principals for the operators (ADR-0031). It reads what
-- decides the tenant and the policy, not who changed them, nor e-mails.
GRANT USAGE ON SCHEMA identity TO erp_control;
GRANT SELECT (tenant_id, id, issuer, audience, tenant_claim, acr_values, amr_values, status) ON identity.identity_provider TO erp_control;
GRANT SELECT (tenant_id, mfa_required, session_max_age, idle_timeout) ON identity.auth_policy TO erp_control;
GRANT SELECT (tenant_id, id, kind, external_issuer, external_subject, display_name, status, version) ON identity.principal TO erp_control;
