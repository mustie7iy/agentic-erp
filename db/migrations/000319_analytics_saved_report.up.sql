-- Saved reports through the API (T95a; ADR-0076 §1-§3; 03-data-model
-- §3.23): analytics.report (000010) had no writer until now. A report is
-- its owner's (owner_id, the identity context's principal: no foreign key
-- leaves the schema, 02 §2.1), active until its owner removes it; removed,
-- it is kept and changes no more (04 §4.3: no DELETE). title and question
-- are the specification's, kept beside it for the list.
--
-- Expand only (06 §6.4): a column with a default, checks the table's rows
-- meet (it has none: nothing wrote it), two permissions; the previous
-- version neither reads nor writes the table.
ALTER TABLE analytics.report
  ADD COLUMN status text NOT NULL DEFAULT 'active' CONSTRAINT report_status_check CHECK (status IN ('active', 'removed')),
  ADD CONSTRAINT report_title_check CHECK (length(btrim(title)) > 0 AND length(title) <= 200),
  ADD CONSTRAINT report_question_check CHECK (question IS NULL OR length(question) <= 2000),
  ADD CONSTRAINT report_spec_check CHECK (jsonb_typeof(spec) = 'object'),
  ADD CONSTRAINT report_queries_check CHECK (jsonb_typeof(queries) = 'object'),
  ADD CONSTRAINT report_version_check CHECK (version >= 1);
-- An owner's reports by title, as their list reads them.
CREATE INDEX report_by_owner ON analytics.report (tenant_id, owner_id, title, id);

-- A removed report is fixed: removing it is its last change.
CREATE FUNCTION analytics.check_report_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'removed' THEN
    RAISE EXCEPTION 'report %: a removed report is not changed (report_removed_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'report_removed_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION analytics.check_report_change() OWNER TO erp_owner;
CREATE TRIGGER report_removed_fixed BEFORE UPDATE ON analytics.report
  FOR EACH ROW EXECUTE FUNCTION analytics.check_report_change();
-- A change sets the status alone: what a report says and whose it is
-- stay as saved.
REVOKE UPDATE ON analytics.report FROM erp_app;
GRANT UPDATE (status, version) ON analytics.report TO erp_app;

-- Reading and running one's saved reports, and saving and removing them
-- (ADR-0076 §3).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('analytics.report.read', 'View and run your saved analytics reports', false),
  ('analytics.report.write', 'Save and remove your analytics reports', false);
