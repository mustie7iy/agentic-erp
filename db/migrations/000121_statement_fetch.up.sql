-- Statement files fetched from a house bank's SFTP channel (T49d; ADR-0047;
-- 03-data-model §3.10): one row per house bank and file content, saying
-- what became of the file. A file imported but not yet moved to the archive
-- directory when a fetch was cut short is moved by the next fetch and never
-- imported twice; a file refused is refused once, with one event, and left
-- on the server for a person to take up with the bank.
CREATE TABLE payments.statement_fetch (
  tenant_id      uuid NOT NULL,
  id             uuid NOT NULL,
  house_bank_id  uuid NOT NULL,
  file_sha256    bytea NOT NULL CHECK (octet_length(file_sha256) = 32),
  file_name      text NOT NULL CHECK (file_name <> ''),  -- as the bank named it in the inbound directory
  status         text NOT NULL CHECK (status IN ('imported', 'archived', 'refused')),
  reason         text,                                    -- why it was refused
  created_at     timestamptz NOT NULL DEFAULT now(),
  created_by     uuid NOT NULL,
  updated_at     timestamptz NOT NULL DEFAULT now(),
  updated_by     uuid NOT NULL,
  version        integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT statement_fetch_file_once UNIQUE (tenant_id, house_bank_id, file_sha256),
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id),
  -- A refusal says why; nothing else has a reason.
  CONSTRAINT statement_fetch_reason CHECK ((status = 'refused') = (reason IS NOT NULL))
);
CREATE TRIGGER statement_fetch_touch BEFORE UPDATE ON payments.statement_fetch FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('payments.statement_fetch'::regclass);

-- The dispatcher starts a fetch for every active house bank whose bank
-- channel names statement directories (settings are never secrets), and
-- the operators' gauges count refused and unarchived files, across tenants.
GRANT SELECT (tenant_id, id, connector_id, status) ON payments.house_bank TO erp_worker;
GRANT SELECT (tenant_id, id, kind, status, settings) ON integration.connector TO erp_worker;
GRANT SELECT (tenant_id, status, updated_at) ON payments.statement_fetch TO erp_worker;
