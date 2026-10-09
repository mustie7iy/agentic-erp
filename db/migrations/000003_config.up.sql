-- Configuration (03-data-model §3.5)

CREATE TABLE config.config_version (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  number        integer NOT NULL CHECK (number > 0),
  document      jsonb NOT NULL,
  hash          bytea NOT NULL CHECK (length(hash) = 32),    -- SHA-256 of the canonical document
  source        text NOT NULL CHECK (source IN ('bootstrap', 'change_request', 'rollback')),
  change_request_id uuid,
  restored_from integer,
  activated_at  timestamptz NOT NULL DEFAULT now(),
  activated_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, number),
  UNIQUE (tenant_id, id),
  CHECK ((source = 'change_request') = (change_request_id IS NOT NULL)),
  CHECK ((source = 'rollback') = (restored_from IS NOT NULL))
);
CREATE TRIGGER config_version_immutable BEFORE UPDATE OR DELETE ON config.config_version
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Exactly one active version per tenant.
CREATE TABLE config.active_version (
  tenant_id  uuid PRIMARY KEY,
  number     integer NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  version    integer NOT NULL DEFAULT 1,
  FOREIGN KEY (tenant_id, number) REFERENCES config.config_version (tenant_id, number)
);
CREATE TRIGGER active_version_touch BEFORE UPDATE ON config.active_version FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE config.change_request (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  number               text NOT NULL,
  title                text NOT NULL CHECK (length(trim(title)) > 0),
  rationale            text NOT NULL DEFAULT '',
  author_id            uuid NOT NULL,                     -- identity context
  base_version         integer NOT NULL,
  proposed             jsonb NOT NULL,
  tier                 smallint NOT NULL CHECK (tier BETWEEN 1 AND 4),
  status               text NOT NULL CHECK (status IN ('draft', 'validating', 'invalid', 'awaiting_evidence', 'pending_approval', 'approved', 'active', 'rejected', 'withdrawn')),
  workflow_request_id  uuid,                               -- workflow context
  activated_version    integer,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, number),
  FOREIGN KEY (tenant_id, base_version) REFERENCES config.config_version (tenant_id, number),
  FOREIGN KEY (tenant_id, activated_version) REFERENCES config.config_version (tenant_id, number),
  CHECK ((status = 'active') = (activated_version IS NOT NULL))
);
CREATE INDEX change_request_by_status ON config.change_request (tenant_id, status, created_at);
CREATE TRIGGER change_request_touch BEFORE UPDATE ON config.change_request FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE config.change_item (
  tenant_id          uuid NOT NULL,
  change_request_id  uuid NOT NULL,
  item_no            integer NOT NULL CHECK (item_no > 0),
  path               text NOT NULL,
  old_value          text,
  new_value          text,
  tier               smallint NOT NULL CHECK (tier BETWEEN 1 AND 4),
  PRIMARY KEY (tenant_id, change_request_id, item_no),
  FOREIGN KEY (tenant_id, change_request_id) REFERENCES config.change_request (tenant_id, id),
  CHECK (old_value IS NOT NULL OR new_value IS NOT NULL)
);
CREATE TRIGGER change_item_immutable BEFORE UPDATE OR DELETE ON config.change_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE config.evidence (
  tenant_id          uuid NOT NULL,
  change_request_id  uuid NOT NULL,
  kind               text NOT NULL CHECK (kind IN ('simulation', 'regression')),
  passed             boolean NOT NULL,
  summary            text NOT NULL,
  detail             jsonb NOT NULL DEFAULT '{}',
  recorded_at        timestamptz NOT NULL DEFAULT now(),
  recorded_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, change_request_id, kind, recorded_at),
  FOREIGN KEY (tenant_id, change_request_id) REFERENCES config.change_request (tenant_id, id)
);
-- Evidence is never overwritten; the latest per kind applies.
CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON config.evidence
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Document number ranges (ADR-0004).
CREATE TABLE config.number_range (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,                  -- tenancy context
  document_type    text NOT NULL CHECK (document_type ~ '^[a-z_]+$'),
  fiscal_year      smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  prefix           text NOT NULL DEFAULT '' CHECK (prefix ~ '^[A-Z0-9-]{0,10}$'),
  first_value      bigint NOT NULL CHECK (first_value > 0),
  last_value       bigint NOT NULL,
  next_value       bigint NOT NULL,
  is_gapless       boolean NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_type, fiscal_year),
  CHECK (last_value >= first_value),
  CHECK (next_value BETWEEN first_value AND last_value + 1)
);
CREATE TRIGGER number_range_touch BEFORE UPDATE ON config.number_range FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Allocates the next number inside the caller's transaction. The row lock
-- makes gapless ranges gapless: a rolled-back transaction releases its number.
CREATE FUNCTION config.next_document_number(p_tenant uuid, p_legal_entity uuid, p_type text, p_year smallint)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE r config.number_range%ROWTYPE;
BEGIN
  SELECT * INTO r FROM config.number_range
  WHERE tenant_id = p_tenant AND legal_entity_id = p_legal_entity AND document_type = p_type AND fiscal_year = p_year
  FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no number range for % in fiscal year %', p_type, p_year USING ERRCODE = 'no_data_found';
  END IF;
  IF r.next_value > r.last_value THEN
    RAISE EXCEPTION 'number range for % in fiscal year % is exhausted', p_type, p_year USING ERRCODE = 'check_violation';
  END IF;
  UPDATE config.number_range SET next_value = r.next_value + 1, version = r.version + 1
  WHERE tenant_id = r.tenant_id AND id = r.id;
  RETURN r.prefix || r.next_value::text;
END $$;

CREATE TABLE config.custom_field_schema (
  tenant_id     uuid NOT NULL,
  entity_type   text NOT NULL CHECK (entity_type ~ '^[a-z_]+\.[a-z_]+$'),
  version       integer NOT NULL CHECK (version > 0),
  json_schema   jsonb NOT NULL,
  change_request_id uuid NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, entity_type, version),
  FOREIGN KEY (tenant_id, change_request_id) REFERENCES config.change_request (tenant_id, id)
);
CREATE TRIGGER custom_field_schema_immutable BEFORE UPDATE OR DELETE ON config.custom_field_schema
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

SELECT platform.create_event_tables('config');
