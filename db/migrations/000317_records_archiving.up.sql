-- Archiving runs (T99b; ADR-0079 §6-§8; 03-data-model §3.32): the records
-- context's schema with its outbox and inbox, and its first table. A run
-- archives one company's journal entries of one fiscal year past
-- retention and closed: queued in the request's transaction, written by
-- the archiver (records.Archiver) to an archive file in object storage
-- with a manifest, and completed with the file's SHA-256, its entries'
-- count and the sum of their debits. Nothing is deleted from the ledger.
-- legal_entity_id is the tenancy context's and requested_by the identity
-- context's: no foreign key leaves the schema (02 §2.1). Expand only: a
-- new schema, a new table and one permission, which the previous version
-- neither reads nor writes (06 §6.4).
CREATE SCHEMA records;
GRANT USAGE ON SCHEMA records TO erp_app, erp_readonly, erp_owner, erp_worker;
SELECT platform.create_event_tables('records');
SELECT platform.secure_table('records.outbox'::regclass);
SELECT platform.secure_table('records.inbox'::regclass);
GRANT UPDATE (published_at) ON records.outbox TO erp_app;
GRANT USAGE ON SCHEMA records TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON records.outbox TO erp_relay;

-- An archiving run (ADR-0079 §6): a company, a document kind (journal
-- entries in the first version) and a fiscal year, with the retention
-- rule's years and the configuration version that put the year past
-- retention when it was asked for. It is queued, then completed with its
-- file, or failed with the problem code that stopped it; either is its
-- last change. A completed run names its object keys, its file's hash and
-- size, and the manifest's counts and debit total in the company's
-- currency.
CREATE TABLE records.archiving_run (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,     -- tenancy context: the company
  document_kind          text NOT NULL CONSTRAINT archiving_run_kind_check CHECK (document_kind IN ('journal_entry')),
  fiscal_year            smallint NOT NULL CONSTRAINT archiving_run_year_check CHECK (fiscal_year BETWEEN 1900 AND 9999),
  retention_years        smallint NOT NULL CONSTRAINT archiving_run_retention_check CHECK (retention_years BETWEEN 0 AND 99),
  config_version         integer NOT NULL CONSTRAINT archiving_run_config_check CHECK (config_version >= 1),
  status                 text NOT NULL DEFAULT 'queued' CONSTRAINT archiving_run_status_check CHECK (status IN ('queued', 'completed', 'failed')),
  object_key             text,              -- the archive file: tenants/<tenant>/archives/<run id>.jsonl
  manifest_key           text,              -- its manifest: tenants/<tenant>/archives/<run id>.manifest.json
  sha256                 bytea,             -- the archive file's SHA-256
  manifest_sha256        bytea,             -- the manifest's SHA-256
  size_bytes             bigint,
  entry_count            integer,
  line_count             integer,
  debit_total            bigint,            -- the sum of the lines' debits in the company's currency, in minor units
  currency               char(3),           -- the company's currency
  failure_code           text,              -- the problem code that failed the run
  completed_at           timestamptz,
  requested_by           uuid NOT NULL,     -- identity context: the person who asked for it
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  version                integer NOT NULL DEFAULT 1 CONSTRAINT archiving_run_version_check CHECK (version >= 1),
  PRIMARY KEY (tenant_id, id),
  -- A completed run names its file, its manifest and their figures, and
  -- when it completed; a run that is not, none of them.
  CONSTRAINT archiving_run_completed_check CHECK ((status = 'completed') = (completed_at IS NOT NULL)
    AND (status = 'completed') = (object_key IS NOT NULL) AND (object_key IS NULL) = (manifest_key IS NULL)
    AND (object_key IS NULL) = (sha256 IS NULL) AND (object_key IS NULL) = (manifest_sha256 IS NULL) AND (object_key IS NULL) = (size_bytes IS NULL)
    AND (object_key IS NULL) = (entry_count IS NULL) AND (object_key IS NULL) = (line_count IS NULL)
    AND (object_key IS NULL) = (debit_total IS NULL) AND (object_key IS NULL) = (currency IS NULL)),
  CONSTRAINT archiving_run_file_check CHECK ((sha256 IS NULL OR octet_length(sha256) = 32) AND (manifest_sha256 IS NULL OR octet_length(manifest_sha256) = 32) AND (size_bytes IS NULL OR size_bytes >= 0)
    AND (entry_count IS NULL OR entry_count >= 0) AND (line_count IS NULL OR line_count >= 0) AND (debit_total IS NULL OR debit_total >= 0)),
  -- A failed run says why; another does not.
  CONSTRAINT archiving_run_failed_check CHECK ((status = 'failed') = (failure_code IS NOT NULL))
);
CREATE TRIGGER archiving_run_touch BEFORE UPDATE ON records.archiving_run FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- One run of a company, kind and year that is queued or completed: a
-- second request answers the first; a failed one may be asked for again.
CREATE UNIQUE INDEX archiving_run_once ON records.archiving_run (tenant_id, legal_entity_id, document_kind, fiscal_year) WHERE status <> 'failed';
-- The archiver claims a tenant's queued runs in order; the list reads the
-- newest first.
CREATE INDEX archiving_run_queued ON records.archiving_run (tenant_id, created_at, id) WHERE status = 'queued';
CREATE INDEX archiving_run_by_created ON records.archiving_run (tenant_id, created_at DESC, id DESC);
SELECT platform.secure_table('records.archiving_run'::regclass);

-- A completed or failed run is fixed: either is its last change.
CREATE FUNCTION records.check_archiving_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'queued' THEN
    RAISE EXCEPTION 'archiving run %: a % run is not changed (archiving_run_settled_fixed)', OLD.id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'archiving_run_settled_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION records.check_archiving_run_change() OWNER TO erp_owner;
CREATE TRIGGER archiving_run_settled_fixed BEFORE UPDATE ON records.archiving_run
  FOR EACH ROW EXECUTE FUNCTION records.check_archiving_run_change();
-- The archiver settles a run; what it is for and who asked stay, and no
-- run is deleted.
REVOKE UPDATE, DELETE ON records.archiving_run FROM erp_app;
GRANT UPDATE (status, object_key, manifest_key, sha256, manifest_sha256, size_bytes, entry_count, line_count, debit_total, currency, failure_code, completed_at,
  version) ON records.archiving_run TO erp_app;

-- The archiver finds, as the worker role across tenants, the tenants with
-- queued runs.
GRANT SELECT (tenant_id, status) ON records.archiving_run TO erp_worker;

-- Starting archiving runs and downloading their files (ADR-0079 §8).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('records.archive.run', 'Start archiving runs of fiscal years past retention and download their files', false);
