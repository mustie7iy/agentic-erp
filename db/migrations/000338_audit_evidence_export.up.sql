-- Audit evidence exports (T101a; ADR-0011 As built, ADR-0016; 03-data-model
-- §3.12): an auditor asks for the evidence of companies and a period, and
-- the evidence exporter (audit.EvidenceExporter) writes it to object
-- storage as files with a manifest: the period's audit events, the
-- companies' journal entries with their lines, the approval decisions on
-- their source documents and the result of verifying the audit chain.
-- The row is the export's request and, once completed, the manifest's
-- SHA-256 and figures; the store holds the bytes (ADR-0016). It lives
-- beside the trail it exports, and like the trail it is kept when a
-- tenant is erased: it holds ids, dates and counts, no personal data.
-- legal_entity_ids are the tenancy context's and requested_by the
-- identity context's: no foreign key leaves the schema (02 §2.1). Expand
-- only: a new table, its grants and one permission, which the previous
-- version neither reads nor writes (06 §6.4).
CREATE TABLE audit.evidence_export (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  -- The companies the export covers: those asked for that the requester
  -- could read when asking, in the order of their codes.
  legal_entity_ids         uuid[] NOT NULL CONSTRAINT evidence_export_companies_check
                             CHECK (cardinality(legal_entity_ids) BETWEEN 1 AND 100 AND array_position(legal_entity_ids, NULL) IS NULL),
  period_from              date NOT NULL,
  period_to                date NOT NULL,
  -- Whether the audit events carry their client addresses: the requester
  -- held audit.event.read when asking (ADR-0011 as amended by T73c).
  with_client_addresses    boolean NOT NULL,
  status                   text NOT NULL DEFAULT 'queued' CONSTRAINT evidence_export_status_check CHECK (status IN ('queued', 'completed', 'failed')),
  manifest_key             text,              -- tenants/<tenant>/audit/evidence/<export id>/manifest.json
  manifest_sha256          bytea,
  files                    jsonb,             -- the manifest's files: [{name, sha256, size_bytes, records}], in the manifest's order
  audit_event_count        integer,
  journal_entry_count      integer,
  approval_decision_count  integer,
  chain_verified           boolean,           -- the chain verified from its first event to chain_last_seq
  chain_last_seq           bigint,            -- the chain's head when the exporter read it
  chain_checked            bigint,            -- the events the verification checked
  chain_anchors_checked    integer,           -- the anchors it checked
  chain_broken_seq         bigint,            -- the first sequence number that did not verify
  failure_code             text,              -- the problem code that failed the export
  completed_at             timestamptz,
  requested_by             uuid NOT NULL,     -- identity context: the person who asked for it
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  version                  integer NOT NULL DEFAULT 1 CONSTRAINT evidence_export_version_check CHECK (version >= 1),
  PRIMARY KEY (tenant_id, id),
  -- A period of one day up to the longest fiscal year, 53 weeks.
  CONSTRAINT evidence_export_period_check CHECK (period_to >= period_from AND period_to - period_from <= 370),
  -- A completed export names its manifest, its files and their figures,
  -- and when it completed; one that is not, none of them.
  CONSTRAINT evidence_export_completed_check CHECK ((status = 'completed') = (completed_at IS NOT NULL)
    AND (status = 'completed') = (manifest_key IS NOT NULL) AND (manifest_key IS NULL) = (manifest_sha256 IS NULL)
    AND (manifest_key IS NULL) = (files IS NULL) AND (manifest_key IS NULL) = (audit_event_count IS NULL)
    AND (manifest_key IS NULL) = (journal_entry_count IS NULL) AND (manifest_key IS NULL) = (approval_decision_count IS NULL)
    AND (manifest_key IS NULL) = (chain_verified IS NULL) AND (manifest_key IS NULL) = (chain_last_seq IS NULL)
    AND (manifest_key IS NULL) = (chain_checked IS NULL) AND (manifest_key IS NULL) = (chain_anchors_checked IS NULL)),
  CONSTRAINT evidence_export_figures_check CHECK ((manifest_sha256 IS NULL OR octet_length(manifest_sha256) = 32)
    AND (files IS NULL OR jsonb_typeof(files) = 'array') AND (audit_event_count IS NULL OR audit_event_count >= 0)
    AND (journal_entry_count IS NULL OR journal_entry_count >= 0) AND (approval_decision_count IS NULL OR approval_decision_count >= 0)
    AND (chain_last_seq IS NULL OR chain_last_seq >= 0) AND (chain_checked IS NULL OR chain_checked >= 0)
    AND (chain_anchors_checked IS NULL OR chain_anchors_checked >= 0)),
  -- A chain that did not verify names where it broke; one that did, nowhere.
  CONSTRAINT evidence_export_chain_check CHECK ((chain_broken_seq IS NOT NULL) = (chain_verified IS NOT NULL AND NOT chain_verified)
    AND (chain_broken_seq IS NULL OR chain_broken_seq > 0)),
  -- A failed export says why; another does not.
  CONSTRAINT evidence_export_failed_check CHECK ((status = 'failed') = (failure_code IS NOT NULL))
);
CREATE TRIGGER evidence_export_touch BEFORE UPDATE ON audit.evidence_export FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The exporter claims a tenant's queued exports in order; the list reads
-- the newest first.
CREATE INDEX evidence_export_queued ON audit.evidence_export (tenant_id, created_at, id) WHERE status = 'queued';
CREATE INDEX evidence_export_by_created ON audit.evidence_export (tenant_id, created_at DESC, id DESC);
SELECT platform.secure_table('audit.evidence_export'::regclass);

-- A completed or failed export is fixed: either is its last change.
CREATE FUNCTION audit.check_evidence_export_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'queued' THEN
    RAISE EXCEPTION 'evidence export %: a % export is not changed (evidence_export_settled_fixed)', OLD.id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'evidence_export_settled_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION audit.check_evidence_export_change() OWNER TO erp_owner;
CREATE TRIGGER evidence_export_settled_fixed BEFORE UPDATE ON audit.evidence_export
  FOR EACH ROW EXECUTE FUNCTION audit.check_evidence_export_change();
-- The exporter settles an export; what it covers and who asked stay, and
-- no export is deleted.
REVOKE UPDATE, DELETE ON audit.evidence_export FROM erp_app;
GRANT UPDATE (status, manifest_key, manifest_sha256, files, audit_event_count, journal_entry_count, approval_decision_count, chain_verified,
  chain_last_seq, chain_checked, chain_anchors_checked, chain_broken_seq, failure_code, completed_at, version) ON audit.evidence_export TO erp_app;

-- The exporter finds, as the worker role across tenants, the tenants with
-- queued exports.
GRANT SELECT (tenant_id, status) ON audit.evidence_export TO erp_worker;

-- Requesting audit evidence exports and downloading their files.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('audit.evidence.export', 'Request audit evidence exports and download their files', false);
