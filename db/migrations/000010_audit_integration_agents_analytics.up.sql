-- Audit (03-data-model §3.12, ADR-0011)

CREATE TABLE audit.audit_event (
  tenant_id    uuid NOT NULL,
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  seq          bigint NOT NULL CHECK (seq > 0),
  actor_id     uuid NOT NULL,
  actor_kind   text NOT NULL CHECK (actor_kind IN ('human', 'agent', 'service', 'system')),
  action       text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
  object_type  text NOT NULL,
  object_id    uuid,
  outcome      text NOT NULL,
  detail       jsonb NOT NULL DEFAULT '{}',
  trace_id     text,
  prev_hash    bytea CHECK (prev_hash IS NULL OR length(prev_hash) = 32),
  hash         bytea NOT NULL CHECK (length(hash) = 32),
  PRIMARY KEY (tenant_id, occurred_at, seq)
) PARTITION BY RANGE (occurred_at);
CREATE UNIQUE INDEX audit_event_seq ON audit.audit_event (tenant_id, seq, occurred_at);
CREATE INDEX audit_event_object ON audit.audit_event (tenant_id, object_type, object_id, occurred_at);
DO $$
DECLARE m date := date '2026-01-01';
BEGIN
  WHILE m < date '2028-01-01' LOOP
    EXECUTE format('CREATE TABLE audit.audit_event_%s PARTITION OF audit.audit_event FOR VALUES FROM (%L) TO (%L)',
                   to_char(m, 'YYYYMM'), m, m + interval '1 month');
    m := m + interval '1 month';
  END LOOP;
END $$;
CREATE TRIGGER audit_event_immutable BEFORE UPDATE OR DELETE ON audit.audit_event FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE audit.chain_head (
  tenant_id   uuid PRIMARY KEY,
  seq         bigint NOT NULL,
  hash        bytea NOT NULL CHECK (length(hash) = 32),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit.anchor (
  tenant_id    uuid NOT NULL,
  seq          bigint NOT NULL,
  hash         bytea NOT NULL CHECK (length(hash) = 32),
  object_ref   text NOT NULL,               -- write-once object storage key
  anchored_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, seq)
);
CREATE TRIGGER anchor_immutable BEFORE UPDATE OR DELETE ON audit.anchor FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Integration (03-data-model §3.13)

CREATE TABLE integration.external_ref (
  tenant_id     uuid NOT NULL,
  entity_type   text NOT NULL CHECK (entity_type ~ '^[a-z_]+\.[a-z_]+$'),   -- e.g. partner.business_partner
  entity_id     uuid NOT NULL,
  system        text NOT NULL CHECK (system ~ '^[A-Z0-9_]{2,20}$'),         -- e.g. SAP_PRD
  object        text NOT NULL,                                              -- e.g. LFA1, ACDOCA
  external_key  text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, system, object, external_key),
  UNIQUE (tenant_id, entity_type, entity_id, system, object)
);

CREATE TABLE integration.connector (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing')),
  name        text NOT NULL,
  settings    jsonb NOT NULL DEFAULT '{}',   -- never secrets
  secret_ref  text,                          -- vault path
  status      text NOT NULL CHECK (status IN ('active', 'disabled')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid NOT NULL,
  version     integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, name)
);
CREATE TRIGGER connector_touch BEFORE UPDATE ON integration.connector FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE integration.message (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  connector_id  uuid NOT NULL,
  direction     text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
  message_type  text NOT NULL,              -- e.g. pain.001, camt.053, X12_850
  status        text NOT NULL CHECK (status IN ('queued', 'sent', 'received', 'processed', 'failed', 'dead')),
  object_type   text,
  object_id     uuid,
  payload_ref   text NOT NULL,              -- object storage key
  attempts      smallint NOT NULL DEFAULT 0,
  last_error    text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, connector_id) REFERENCES integration.connector (tenant_id, id)
);
CREATE INDEX message_pending ON integration.message (tenant_id, status, created_at) WHERE status IN ('queued', 'failed');
CREATE TRIGGER message_touch BEFORE UPDATE ON integration.message FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE integration.output_message (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  object_type  text NOT NULL CHECK (object_type IN ('purchase_order')),
  object_id    uuid NOT NULL,
  channel      text NOT NULL CHECK (channel IN ('email', 'edi', 'print')),
  recipient    text NOT NULL,
  subject      text NOT NULL,
  body_ref     text NOT NULL,
  message_id   uuid,                          -- integration.message once transmitted
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX output_message_by_object ON integration.output_message (tenant_id, object_type, object_id);

CREATE TABLE integration.migration_run (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  connector_id  uuid NOT NULL,
  scope         jsonb NOT NULL,              -- legal entities and fiscal year
  mode          text NOT NULL CHECK (mode IN ('documents', 'balances')),
  is_rehearsal  boolean NOT NULL,
  status        text NOT NULL CHECK (status IN ('queued', 'running', 'loaded', 'reconciled', 'failed')),
  report        jsonb,
  operation_id  uuid,
  started_at    timestamptz,
  finished_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, connector_id) REFERENCES integration.connector (tenant_id, id)
);
CREATE TRIGGER migration_run_touch BEFORE UPDATE ON integration.migration_run FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Agents (03-data-model §3.14)

CREATE TABLE agents.session (
  tenant_id      uuid NOT NULL,
  id             uuid NOT NULL,
  agent_id       uuid NOT NULL,              -- identity.principal of kind agent
  purpose        text NOT NULL,
  model          text NOT NULL,
  operation_id   uuid,
  status         text NOT NULL CHECK (status IN ('running', 'completed', 'failed', 'refused')),
  input_tokens   bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  output_tokens  bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  cost           bigint NOT NULL DEFAULT 0 CHECK (cost >= 0),
  cost_currency  char(3) NOT NULL REFERENCES ref.currency,
  started_at     timestamptz NOT NULL DEFAULT now(),
  ended_at       timestamptz,
  version        integer NOT NULL DEFAULT 1,
  updated_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX session_by_agent ON agents.session (tenant_id, agent_id, started_at);
CREATE TRIGGER session_touch BEFORE UPDATE ON agents.session FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE agents.tool_call (
  tenant_id       uuid NOT NULL,
  session_id      uuid NOT NULL,
  seq             integer NOT NULL CHECK (seq > 0),
  tool            text NOT NULL,
  input           jsonb NOT NULL,
  result_summary  text,
  is_error        boolean NOT NULL DEFAULT false,
  called_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, session_id, seq),
  FOREIGN KEY (tenant_id, session_id) REFERENCES agents.session (tenant_id, id)
);
CREATE TRIGGER tool_call_immutable BEFORE UPDATE OR DELETE ON agents.tool_call FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE agents.recommendation (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  session_id   uuid NOT NULL,
  object_type  text NOT NULL,
  object_id    uuid NOT NULL,
  kind         text NOT NULL,
  content      text NOT NULL,
  outcome      text CHECK (outcome IN ('accepted', 'overridden', 'ignored')),   -- what people decided
  decided_by   uuid,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  version      integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, session_id) REFERENCES agents.session (tenant_id, id),
  CHECK ((outcome IS NULL) = (decided_by IS NULL))
);
CREATE TRIGGER recommendation_touch BEFORE UPDATE ON agents.recommendation FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Analytics: saved reports (read models are projections, defined later)

CREATE TABLE analytics.report (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  title       text NOT NULL,
  question    text,
  spec        jsonb NOT NULL,
  queries     jsonb NOT NULL,
  owner_id    uuid NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  version     integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER report_touch BEFORE UPDATE ON analytics.report FOR EACH ROW EXECUTE FUNCTION platform.touch();

SELECT platform.create_event_tables('integration');
SELECT platform.create_event_tables('agents');
