-- A saved report shared with people or roles (T95c; ADR-0076 §7;
-- 03-data-model §3.23): each viewer reads and runs it with their own
-- access; a kept run names the companies it covered, and is read only by
-- whoever's access covers them. principal_id and role_id are the identity
-- context's: no foreign key leaves the schema (02 §2.1).
--
-- Expand only (06 §6.4): a table and a nullable column the previous
-- version neither reads nor writes. Runs kept before have no companies
-- and stay their owner's alone.
CREATE TABLE analytics.report_share (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  report_id     uuid NOT NULL,
  principal_id  uuid,                -- identity context: a person
  role_id       uuid,                -- identity context: a role
  status        text NOT NULL DEFAULT 'active' CONSTRAINT report_share_status_check CHECK (status IN ('active', 'ended')),
  version       integer NOT NULL DEFAULT 1 CONSTRAINT report_share_version_check CHECK (version >= 1),
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT report_share_report_fkey FOREIGN KEY (tenant_id, report_id) REFERENCES analytics.report (tenant_id, id),
  -- A share is with a person or with a role, never both.
  CONSTRAINT report_share_grantee CHECK ((principal_id IS NULL) <> (role_id IS NULL))
);
-- A report is shared with a person or a role once at a time.
CREATE UNIQUE INDEX report_share_once ON analytics.report_share (tenant_id, report_id, coalesce(principal_id, role_id)) WHERE status = 'active';
-- What a person reads: the active shares with them or their roles.
CREATE INDEX report_share_principal ON analytics.report_share (tenant_id, principal_id, report_id) WHERE status = 'active' AND principal_id IS NOT NULL;
CREATE INDEX report_share_role ON analytics.report_share (tenant_id, role_id, report_id) WHERE status = 'active' AND role_id IS NOT NULL;
-- A report's shares, as the owner lists them.
CREATE INDEX report_share_by_report ON analytics.report_share (tenant_id, report_id, created_at, id);

-- An ended share is fixed: ending it is its last change.
CREATE FUNCTION analytics.check_report_share_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'ended' THEN
    RAISE EXCEPTION 'report share %: an ended share is not changed (report_share_ended_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'report_share_ended_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION analytics.check_report_share_change() OWNER TO erp_owner;
CREATE TRIGGER report_share_ended_fixed BEFORE UPDATE ON analytics.report_share
  FOR EACH ROW EXECUTE FUNCTION analytics.check_report_share_change();
-- Optimistic locking (ADR-0008); updated_at is the time a share ended.
CREATE TRIGGER report_share_touch BEFORE UPDATE ON analytics.report_share
  FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('analytics.report_share'::regclass);
-- A change ends a share and nothing else (secure_table grants no delete).
REVOKE UPDATE ON analytics.report_share FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON analytics.report_share TO erp_app;

-- The company codes a kept run covered: its owner's company access when
-- it ran; none for a failed run, which kept no results.
ALTER TABLE analytics.report_run
  ADD COLUMN companies text[] CONSTRAINT report_run_companies_check CHECK (array_position(companies, NULL) IS NULL AND cardinality(companies) <= 10000);
