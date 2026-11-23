-- The WMS connector (S2q1; ADR-0082 As built S2q; 03-data-model §3.13):
-- an external warehouse system a tenant connects as an integration
-- connector of the kind wms, for the plants its settings name. The ERP
-- sends it each outbound delivery created at such a plant to pick, and
-- polls it for the goods issues it reports back, which it posts once per
-- message id. Both directions are rows of integration.wms_message: an
-- outbound row is the outbox of its call (ADR-0007), written by the bus
-- consumer that reads the delivery's event and sent after that commit by
-- the exchange worker; an inbound row is the inbox of a confirmation,
-- written in the transaction that posts it, so a confirmation read twice
-- is posted once. integration.message (000010) is not used: its payloads
-- live in object storage, and a WMS message is a small JSON body kept in
-- its row. document_id and legal_entity_id are the shipping and tenancy
-- contexts': no foreign key leaves the schema (02 §2.1). Expand only: a
-- new table, the connector kind check widened NOT VALID (validated by
-- 000450), which the previous version neither reads nor writes.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms')) NOT VALID;

-- A message exchanged with a WMS. Outbound (kind outbound_delivery): the
-- delivery sent to pick, its body the message as sent, pending until the
-- WMS took it (sent) or the last attempt failed (failed); its message_id is
-- its id, which the WMS deduplicates on. Inbound (kind goods_issue): a
-- movement the WMS reported back under its own message id, posted (naming
-- the material document) or refused (naming the problem), and
-- acknowledged to the WMS after the commit that recorded it.
CREATE TABLE integration.wms_message (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  connector_id             uuid NOT NULL,
  direction                text NOT NULL CHECK (direction IN ('outbound', 'inbound')),
  message_id               text NOT NULL CHECK (length(message_id) BETWEEN 1 AND 200),
  kind                     text NOT NULL CHECK (kind IN ('outbound_delivery', 'goods_issue')),
  legal_entity_id          uuid,          -- tenancy context
  document_id              uuid,          -- shipping context: the outbound delivery
  document_number          text CHECK (length(document_number) BETWEEN 1 AND 40),
  plant_code               text CHECK (length(plant_code) BETWEEN 1 AND 20),
  body                     jsonb NOT NULL,
  status                   text NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'posted', 'refused')),
  attempts                 integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
  next_attempt_at          timestamptz,
  last_error               text CHECK (length(last_error) BETWEEN 1 AND 100),  -- the last failed attempt's problem code
  sent_at                  timestamptz,
  problem_code             text CHECK (length(problem_code) BETWEEN 1 AND 100),
  problem_detail           text CHECK (length(problem_detail) BETWEEN 1 AND 1000),
  material_document_id     uuid,          -- inventory context
  material_document_number text CHECK (length(material_document_number) BETWEEN 1 AND 40),
  acknowledged_at          timestamptz,
  version                  integer NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, connector_id, direction, message_id),
  FOREIGN KEY (tenant_id, connector_id) REFERENCES integration.connector (tenant_id, id),
  -- Each direction has its own kinds and statuses.
  CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind = 'outbound_delivery')
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))),
  -- An outbound message names its delivery, company and plant and has
  -- none of an inbound one's outcome; only a pending one is unattempted.
  CONSTRAINT wms_message_outbound CHECK (direction = 'inbound' OR (legal_entity_id IS NOT NULL AND document_id IS NOT NULL
    AND document_number IS NOT NULL AND plant_code IS NOT NULL AND problem_code IS NULL AND problem_detail IS NULL
    AND material_document_id IS NULL AND acknowledged_at IS NULL AND (status = 'pending' OR attempts >= 1))),
  CONSTRAINT wms_message_pending CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
  CONSTRAINT wms_message_sent CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
  -- An inbound message is never attempted; a refused one names its
  -- problem, a posted one its delivery, company and material document.
  CONSTRAINT wms_message_inbound CHECK (direction = 'outbound' OR (attempts = 0 AND last_error IS NULL
    AND (status = 'refused') = (problem_code IS NOT NULL) AND (problem_code IS NULL) = (problem_detail IS NULL)
    AND (status = 'posted') = (material_document_id IS NOT NULL) AND (material_document_id IS NULL) = (material_document_number IS NULL)
    AND (status = 'refused' OR (document_id IS NOT NULL AND legal_entity_id IS NOT NULL))))
);
-- A delivery is sent to a WMS once.
CREATE UNIQUE INDEX wms_message_once ON integration.wms_message (tenant_id, kind, document_id) WHERE direction = 'outbound';
-- The exchange picks the pending messages that are due.
CREATE INDEX wms_message_due ON integration.wms_message (next_attempt_at) WHERE status = 'pending';
CREATE TRIGGER wms_message_touch BEFORE UPDATE ON integration.wms_message FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- What a message is never changes; an inbound message's outcome stays as
-- recorded, only its acknowledgement is added.
CREATE FUNCTION integration.wms_message_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.connector_id, NEW.direction, NEW.message_id, NEW.kind, NEW.legal_entity_id, NEW.document_id, NEW.document_number, NEW.plant_code, NEW.body)
       IS DISTINCT FROM (OLD.connector_id, OLD.direction, OLD.message_id, OLD.kind, OLD.legal_entity_id, OLD.document_id, OLD.document_number,
         OLD.plant_code, OLD.body)
     OR (OLD.direction = 'inbound' AND (NEW.status, NEW.problem_code, NEW.problem_detail, NEW.material_document_id, NEW.material_document_number)
       IS DISTINCT FROM (OLD.status, OLD.problem_code, OLD.problem_detail, OLD.material_document_id, OLD.material_document_number))
     OR (OLD.acknowledged_at IS NOT NULL AND NEW.acknowledged_at IS DISTINCT FROM OLD.acknowledged_at)
     OR (OLD.status IN ('sent', 'failed') AND NEW.status IS DISTINCT FROM OLD.status) THEN
    RAISE EXCEPTION 'wms_message_kept: message %: what it is and its outcome stay as recorded', OLD.message_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'wms_message_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION integration.wms_message_kept() OWNER TO erp_owner;
CREATE TRIGGER wms_message_kept BEFORE UPDATE ON integration.wms_message FOR EACH ROW EXECUTE FUNCTION integration.wms_message_kept();
SELECT platform.secure_table('integration.wms_message'::regclass);
-- The exchange finds the tenants with messages due across tenants.
GRANT SELECT (tenant_id, status, next_attempt_at) ON integration.wms_message TO erp_worker;
