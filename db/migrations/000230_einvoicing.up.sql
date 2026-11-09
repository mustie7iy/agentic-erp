-- E-invoices fetched from the tenant's accredited provider (T65a; ADR-0061
-- §2, §4, §7, §11; 03-data-model §3.25): one row per document the
-- provider delivered through an einvoice_provider connector, recorded
-- once per connector and the provider's message id, its bytes kept
-- write-once in object storage under their SHA-256 (ADR-0016). T65a only
-- records documents: the status trigger allows received alone, and the
-- later slices widen it as they add processing (T65c refused, T65d entered
-- and held, T65f dismissed). connector_id is the integration context's,
-- legal_entity_id the tenancy context's, fetch_operation_id the
-- platform's: no foreign key leaves the schema (02 §2.1). Expand only: a
-- new schema, the connector kind check widened NOT VALID to the new kind
-- einvoice_provider (06 §6.4, validated by 000231) and two permissions,
-- which the previous version neither reads nor writes.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider')) NOT VALID;

CREATE SCHEMA einvoicing;
GRANT USAGE ON SCHEMA einvoicing TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('einvoicing');
SELECT platform.secure_table('einvoicing.outbox'::regclass);
SELECT platform.secure_table('einvoicing.inbox'::regclass);
GRANT UPDATE (published_at) ON einvoicing.outbox TO erp_app;
GRANT USAGE ON SCHEMA einvoicing TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON einvoicing.outbox TO erp_relay;

-- A document as the provider delivered it. receiver is the participant
-- identifier the provider listed it for, legal_entity_id the connector's
-- company of that participant when there is one (a document addressed to
-- none is refused by T65c, einvoicing.not_addressed_to_company).
-- acknowledged_at is set once the provider was told it is kept, after the
-- commit that recorded it (§4).
CREATE TABLE einvoicing.einvoice (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  connector_id        uuid NOT NULL,       -- integration context
  message_id          text NOT NULL CHECK (length(message_id) BETWEEN 1 AND 200),
  receiver            text CHECK (length(receiver) BETWEEN 1 AND 200),
  legal_entity_id     uuid,                -- tenancy context
  status              text NOT NULL CHECK (status IN ('received')),
  sha256              bytea NOT NULL CHECK (length(sha256) = 32),
  size                bigint NOT NULL CHECK (size > 0),
  content_ref         text NOT NULL,       -- object key: tenants/<tenant>/documents/<sha256>
  fetch_operation_id  uuid NOT NULL,       -- platform.operation
  received_at         timestamptz NOT NULL DEFAULT now(),
  acknowledged_at     timestamptz,
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- A message is recorded once (§4: ON CONFLICT DO NOTHING).
  CONSTRAINT einvoice_message_once UNIQUE (tenant_id, connector_id, message_id),
  CONSTRAINT einvoice_acknowledged CHECK (acknowledged_at IS NULL OR acknowledged_at >= received_at)
);
CREATE TRIGGER einvoice_touch BEFORE UPDATE ON einvoicing.einvoice FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list, newest received first, and a fetch's documents.
CREATE INDEX einvoice_by_received ON einvoicing.einvoice (tenant_id, received_at, id);
CREATE INDEX einvoice_by_fetch ON einvoicing.einvoice (tenant_id, fetch_operation_id);
SELECT platform.secure_table('einvoicing.einvoice'::regclass);
-- Recording and acknowledging are the only writes: what was recorded
-- never changes, and nothing is deleted.
REVOKE UPDATE ON einvoicing.einvoice FROM erp_app;
GRANT UPDATE (acknowledged_at, version, updated_at, updated_by) ON einvoicing.einvoice TO erp_app;

-- A document is recorded received, and its status moves only as the
-- slices that process it allow (T65a: none); what was recorded is fixed,
-- and its acknowledgement is set once.
CREATE FUNCTION einvoicing.check_einvoice() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'received' THEN
      RAISE EXCEPTION 'an e-invoice is recorded received, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status THEN
    RAISE EXCEPTION 'e-invoice % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_transition';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.connector_id, NEW.message_id, NEW.receiver, NEW.legal_entity_id, NEW.sha256, NEW.size, NEW.content_ref,
      NEW.fetch_operation_id, NEW.received_at, NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.connector_id,
      OLD.message_id, OLD.receiver, OLD.legal_entity_id, OLD.sha256, OLD.size, OLD.content_ref, OLD.fetch_operation_id, OLD.received_at,
      OLD.created_at, OLD.created_by)
      OR (OLD.acknowledged_at IS NOT NULL AND NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at) THEN
    RAISE EXCEPTION 'e-invoice % keeps what was recorded and its acknowledgement', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'einvoice_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION einvoicing.check_einvoice() OWNER TO erp_owner;
CREATE TRIGGER einvoice_transition BEFORE INSERT OR UPDATE ON einvoicing.einvoice FOR EACH ROW EXECUTE FUNCTION einvoicing.check_einvoice();

-- Reading and fetching e-invoices (ADR-0061 §11).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('einvoicing.einvoice.read', 'View e-invoices received from the accredited provider of the companies one may act in', false),
  ('einvoicing.einvoice.manage', 'Fetch e-invoices from the accredited provider and resolve held ones', false);
