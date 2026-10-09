-- Tenancy and organization (03-data-model §3.3)

CREATE TABLE tenancy.tenant (
  id              uuid PRIMARY KEY,
  name            text NOT NULL CHECK (length(trim(name)) > 0),
  region          text NOT NULL CHECK (region ~ '^[a-z]{2}(-[a-z0-9]+)*$'),
  group_currency  char(3) NOT NULL REFERENCES ref.currency,
  status          text NOT NULL CHECK (status IN ('provisioning', 'active', 'suspended', 'offboarded')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  version         integer NOT NULL DEFAULT 1
);
CREATE TRIGGER tenant_touch BEFORE UPDATE ON tenancy.tenant FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE tenancy.legal_entity (
  tenant_id           uuid NOT NULL REFERENCES tenancy.tenant,
  id                  uuid NOT NULL,
  code                text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,10}$'),
  name                text NOT NULL CHECK (length(trim(name)) > 0),
  country_code        char(2) NOT NULL REFERENCES ref.country,
  local_currency      char(3) NOT NULL REFERENCES ref.currency,
  fiscal_calendar_id  uuid NOT NULL,               -- ledger context
  status              text NOT NULL CHECK (status IN ('active', 'blocked', 'inactive')),
  extensions          jsonb NOT NULL DEFAULT '{}',
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  version             integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, code)
);
CREATE TRIGGER legal_entity_touch BEFORE UPDATE ON tenancy.legal_entity FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE tenancy.org_unit (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  kind             text NOT NULL CHECK (kind IN ('purchasing_org', 'plant', 'storage_location', 'sales_org')),
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9_]{1,10}$'),
  name             text NOT NULL,
  parent_id        uuid,
  status           text NOT NULL CHECK (status IN ('active', 'blocked', 'inactive')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, kind, code),
  FOREIGN KEY (tenant_id, legal_entity_id) REFERENCES tenancy.legal_entity (tenant_id, id),
  FOREIGN KEY (tenant_id, parent_id) REFERENCES tenancy.org_unit (tenant_id, id)
);
CREATE TRIGGER org_unit_touch BEFORE UPDATE ON tenancy.org_unit FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.create_event_tables('tenancy');

-- Identity and access (03-data-model §3.4)

CREATE TABLE identity.principal (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  kind              text NOT NULL CHECK (kind IN ('human', 'agent', 'service')),
  external_subject  text,                           -- IdP subject for humans and services
  display_name      text NOT NULL,
  email             text CHECK (email IS NULL OR email ~ '^[^@\s]+@[^@\s]+$'),
  all_companies     boolean NOT NULL DEFAULT false,
  status            text NOT NULL CHECK (status IN ('active', 'blocked', 'deprovisioned')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  version           integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CHECK (kind = 'agent' OR external_subject IS NOT NULL)
);
CREATE UNIQUE INDEX principal_subject ON identity.principal (tenant_id, external_subject) WHERE external_subject IS NOT NULL;
CREATE TRIGGER principal_touch BEFORE UPDATE ON identity.principal FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE identity.agent_profile (
  tenant_id       uuid NOT NULL,
  principal_id    uuid NOT NULL,
  autonomy_level  smallint NOT NULL DEFAULT 0 CHECK (autonomy_level BETWEEN 0 AND 3),
  model           text NOT NULL,
  monthly_budget  bigint CHECK (monthly_budget >= 0),          -- minor units of budget_currency
  budget_currency char(3) REFERENCES ref.currency,
  description     text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  updated_by      uuid NOT NULL,
  version         integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, principal_id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES identity.principal (tenant_id, id),
  CHECK ((monthly_budget IS NULL) = (budget_currency IS NULL))
);
CREATE TRIGGER agent_profile_touch BEFORE UPDATE ON identity.agent_profile FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE identity.role (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[A-Za-z0-9_]{1,40}$'),
  name         text NOT NULL,
  description  text,
  status       text NOT NULL CHECK (status IN ('active', 'inactive')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  version      integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, code)
);
CREATE TRIGGER role_touch BEFORE UPDATE ON identity.role FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Grants below are effective-dated assignments: inserted, then ended through
-- platform.end_validity(); never updated otherwise or deleted, so the access a
-- principal had on any past date can be reconstructed for audit.
CREATE TABLE identity.role_permission (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  role_id          uuid NOT NULL,
  permission_code  text NOT NULL REFERENCES ref.permission,
  valid_from       date NOT NULL,
  valid_to         date,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  ended_at         timestamptz,
  ended_by         uuid,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, role_id) REFERENCES identity.role (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, role_id WITH =, permission_code WITH =, daterange(valid_from, valid_to) WITH &&)
);

CREATE TABLE identity.principal_role (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  principal_id  uuid NOT NULL,
  role_id       uuid NOT NULL,
  valid_from    date NOT NULL,
  valid_to      date,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  ended_at      timestamptz,
  ended_by      uuid,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES identity.principal (tenant_id, id),
  FOREIGN KEY (tenant_id, role_id) REFERENCES identity.role (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, principal_id WITH =, role_id WITH =, daterange(valid_from, valid_to) WITH &&)
);
CREATE INDEX principal_role_by_role ON identity.principal_role (tenant_id, role_id);

CREATE TABLE identity.company_access (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  principal_id     uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,                  -- tenancy context
  valid_from       date NOT NULL,
  valid_to         date,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  ended_at         timestamptz,
  ended_by         uuid,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES identity.principal (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, principal_id WITH =, legal_entity_id WITH =, daterange(valid_from, valid_to) WITH &&)
);

CREATE TABLE identity.sod_rule (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  permission_a  text NOT NULL REFERENCES ref.permission,
  permission_b  text NOT NULL REFERENCES ref.permission,
  description   text NOT NULL,
  valid_from    date NOT NULL,
  valid_to      date,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  ended_at      timestamptz,
  ended_by      uuid,
  PRIMARY KEY (tenant_id, id),
  CHECK (permission_a < permission_b),              -- one canonical order per pair
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, permission_a WITH =, permission_b WITH =, daterange(valid_from, valid_to) WITH &&)
);

CREATE TRIGGER role_permission_end BEFORE UPDATE ON identity.role_permission FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
CREATE TRIGGER principal_role_end BEFORE UPDATE ON identity.principal_role FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
CREATE TRIGGER company_access_end BEFORE UPDATE ON identity.company_access FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
CREATE TRIGGER sod_rule_end BEFORE UPDATE ON identity.sod_rule FOR EACH ROW EXECUTE FUNCTION platform.end_validity();

-- Agents never hold approval permissions (ADR-0010). Checked whenever a role
-- is granted to an agent and whenever a permission is added to a role.
CREATE FUNCTION identity.check_agent_approvals() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE offending text;
BEGIN
  SELECT rp.permission_code INTO offending
  FROM identity.principal_role pr
  JOIN identity.principal p ON p.tenant_id = pr.tenant_id AND p.id = pr.principal_id AND p.kind = 'agent'
  JOIN identity.role_permission rp ON rp.tenant_id = pr.tenant_id AND rp.role_id = pr.role_id
  JOIN ref.permission perm ON perm.code = rp.permission_code AND perm.is_approval
  WHERE pr.tenant_id = NEW.tenant_id AND pr.role_id = NEW.role_id
    -- current or future grants on both sides overlap in time
    AND daterange(pr.valid_from, pr.valid_to) && daterange(rp.valid_from, rp.valid_to)
    AND (pr.valid_to IS NULL OR pr.valid_to > current_date)
  LIMIT 1;
  IF offending IS NOT NULL THEN
    RAISE EXCEPTION 'agents may not hold approval permission %', offending USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER principal_role_agent_approvals AFTER INSERT ON identity.principal_role
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION identity.check_agent_approvals();
CREATE CONSTRAINT TRIGGER role_permission_agent_approvals AFTER INSERT ON identity.role_permission
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION identity.check_agent_approvals();

SELECT platform.create_event_tables('identity');
