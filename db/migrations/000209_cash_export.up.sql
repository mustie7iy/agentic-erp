-- The treasury connector (T53h; ADR-0051 §14, decision P8, §15;
-- ADR-0038; 03-data-model §3.18): a snapshot of the liquidity forecast
-- exported in the cash export schema v1 (api/schemas/cash-export.v1.json)
-- to a treasury system's SFTP directory, through an integration connector
-- of the new kind treasury_export. An export is queued for every active
-- treasury_export connector naming the company when its snapshot is taken,
-- and again when a person asks for one; the exporter (cash.Exporter)
-- claims it, uploads with no transaction open and records the outcome:
-- delivered, or tried again after a wait, or failed. The file is named by
-- company, snapshot date and content hash, so a resend is the same file
-- under the same name and is recognised: resend says the connector took
-- that content before. An export goes with its snapshot when retention
-- deletes it (ON DELETE CASCADE, run by cash.delete_expired_snapshots as
-- the tables' owner); the application deletes nothing. connector_id is the
-- integration context's and legal_entity_id the tenancy context's: no
-- foreign key crosses a schema. Expand only: a new table, the connector
-- kind check widened NOT VALID (06 §6.4, validated by 000210) and two
-- permissions, which the previous version neither reads nor writes.

ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export')) NOT VALID;

CREATE TABLE cash.export (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  snapshot_id      uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,     -- tenancy context: the snapshot's company
  connector_id     uuid NOT NULL,     -- integration context: a treasury_export connector
  format           text NOT NULL CHECK (format IN ('jsonl', 'csv')),
  status           text NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
  attempts         smallint NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
  next_attempt_at  timestamptz,       -- pending: when the exporter next claims it
  file_name        text,              -- set when first claimed: cash-<company>-<snapshot date>-<sha256>.<format>
  sha256           bytea,             -- the file's content hash
  resend           boolean,           -- delivered: the connector had taken this content before
  last_error       text,              -- the problem code of the last failed attempt
  delivered_at     timestamptz,
  requested_by     uuid NOT NULL,     -- who took the snapshot, or asked for the export
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT export_snapshot FOREIGN KEY (tenant_id, snapshot_id, legal_entity_id)
    REFERENCES cash.snapshot (tenant_id, id, legal_entity_id) ON DELETE CASCADE,
  -- Only a pending export waits for a claim.
  CONSTRAINT export_pending CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
  -- A delivered export names its file, when it was delivered and whether it was a resend.
  CONSTRAINT export_delivered CHECK ((status = 'delivered') = (delivered_at IS NOT NULL)
    AND (status = 'delivered') = (resend IS NOT NULL) AND (status <> 'delivered' OR file_name IS NOT NULL)),
  CONSTRAINT export_file CHECK ((file_name IS NULL) = (sha256 IS NULL)
    AND (sha256 IS NULL OR octet_length(sha256) = 32)
    AND (file_name IS NULL OR file_name ~ ('^cash-[A-Z0-9]{1,10}-[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9a-f]{64}\.' || format || '$')))
);
CREATE TRIGGER export_touch BEFORE UPDATE ON cash.export FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The exporter claims a tenant's due exports in order; a snapshot's exports
-- are listed by it; a resend is found by connector and content hash.
CREATE INDEX export_due ON cash.export (tenant_id, next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX export_by_snapshot ON cash.export (tenant_id, snapshot_id, created_at, id);
CREATE INDEX export_delivered_file ON cash.export (tenant_id, connector_id, sha256) WHERE status = 'delivered';
SELECT platform.secure_table('cash.export'::regclass);

-- The exporter finds, as the worker role across tenants, the tenants with
-- exports due.
GRANT SELECT (tenant_id, status, next_attempt_at) ON cash.export TO erp_worker;

-- Reading exports, and asking for one (ADR-0051 §15): a person's, never an
-- agent's (x-agent: false).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.export.read', 'View the exports of liquidity forecast snapshots to treasury systems', false),
  ('cash.export.send', 'Export a liquidity forecast snapshot to a treasury system', false);
