-- The control matrix (T98b; ADR-0078 §7-§9; 03-data-model §3.30): a
-- control's links to what enforces it in the product (a permission, a
-- segregation-of-duties rule, a path of the configuration) and its
-- references to a framework the tenant names. What a link names is
-- another context's (identity, configuration) or the platform's
-- permission catalog: no foreign key leaves the schema (02 §2.1), and a
-- link whose target is gone stays, shown as broken by the matrix, which
-- is computed on read. Expand only: a column with a default and a new
-- table, which the previous version neither reads nor writes (06 §6.4).

-- A control's framework references are valid: at most 50 texts of 1 to
-- 200 characters, none null, none with white space around it, none
-- twice.
CREATE FUNCTION grc.framework_references_valid(refs text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
  SELECT cardinality(refs) <= 50
    AND NOT EXISTS (SELECT 1 FROM unnest(refs) r WHERE r IS NULL OR length(r) NOT BETWEEN 1 AND 200 OR r <> btrim(r))
    AND cardinality(refs) = (SELECT count(DISTINCT r) FROM unnest(refs) r)
$$;
ALTER FUNCTION grc.framework_references_valid(text[]) OWNER TO erp_owner;

-- The references of a framework the tenant names (ADR-0078 §9): free
-- text of the tenant's, in the order it gives them. No framework's text
-- or numbering is shipped.
ALTER TABLE grc.control
  ADD COLUMN framework_references text[] NOT NULL DEFAULT '{}'
    CONSTRAINT control_framework_references_check CHECK (grc.framework_references_valid(framework_references));
GRANT UPDATE (framework_references) ON grc.control TO erp_app;

-- A link from a control to what enforces it (ADR-0078 §7). target is the
-- permission's code, the segregation-of-duties rule's id or the
-- configuration path, by kind. A link is active or removed; it is
-- removed once, by a person or an agent, and never deleted: a link that
-- no longer holds is not removed for that.
CREATE TABLE grc.control_link (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  control_id  uuid NOT NULL,
  kind        text NOT NULL CONSTRAINT control_link_kind_check CHECK (kind IN ('permission', 'sod_rule', 'configuration_path')),
  target      text NOT NULL,
  status      text NOT NULL DEFAULT 'active' CONSTRAINT control_link_status_check CHECK (status IN ('active', 'removed')),
  version     integer NOT NULL DEFAULT 1 CONSTRAINT control_link_version_check CHECK (version >= 1),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT control_link_control_fkey FOREIGN KEY (tenant_id, control_id) REFERENCES grc.control (tenant_id, id),
  -- A permission code as the catalog writes one, a rule's id, or a path
  -- of 1 to 500 characters without white space around it.
  CONSTRAINT control_link_target_check CHECK (CASE kind
    WHEN 'permission' THEN target ~ '^[a-z_]+(\.[a-z_]+)+$'
    WHEN 'sod_rule' THEN target ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    ELSE length(target) BETWEEN 1 AND 500 AND target = btrim(target) END)
);
CREATE TRIGGER control_link_touch BEFORE UPDATE ON grc.control_link FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A control has one active link to a target; a removed link frees it.
CREATE UNIQUE INDEX control_link_once ON grc.control_link (tenant_id, control_id, kind, target) WHERE status = 'active';
-- A control's links in the order they were made, which the matrix shows.
CREATE INDEX control_link_by_control ON grc.control_link (tenant_id, control_id, id);
SELECT platform.secure_table('grc.control_link'::regclass);

-- A removed link is fixed: removing is its last change.
CREATE FUNCTION grc.check_control_link_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'removed' THEN
    RAISE EXCEPTION 'control link %: a removed link is not changed (control_link_removed_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'control_link_removed_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION grc.check_control_link_change() OWNER TO erp_owner;
CREATE TRIGGER control_link_removed_fixed BEFORE UPDATE ON grc.control_link
  FOR EACH ROW EXECUTE FUNCTION grc.check_control_link_change();
-- Removing is the only change: what a link names stays, and no link is
-- deleted.
REVOKE UPDATE ON grc.control_link FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON grc.control_link TO erp_app;
