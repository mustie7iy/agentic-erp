-- Material governance (S1b; ADR-0082 §9; 03-data-model §3.34): creating
-- or changing a material is a material change approved through the
-- approvals context, as a business partner's is. A material is pending
-- until its create change is approved; the change lists the materials of
-- the same description as possible duplicates. Expand only (06 §6.4): a
-- table, a function, an index and a status the previous version neither
-- writes nor reads; the widened status check is added NOT VALID and
-- validated by 000365 (every row already holds it).
ALTER TABLE materials.material DROP CONSTRAINT material_status_check,
  ADD CONSTRAINT material_status_check CHECK (status IN ('pending', 'active', 'blocked')) NOT VALID;

-- A text as duplicates are matched by it: white space trimmed and each run
-- of it one space, in capitals; partner.normalized_name's rule (000330),
-- here for every context's use (TestNormalizedText holds them equal).
CREATE FUNCTION platform.normalized_text(t text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
RETURN upper(btrim(regexp_replace(t, '[\s\u0085   -     　]+', ' ', 'g'), ' '));
GRANT EXECUTE ON FUNCTION platform.normalized_text(text) TO erp_app, erp_readonly;

-- The duplicate check's index: the tenant's materials by description, as
-- the check matches them.
CREATE INDEX material_normalized_description ON materials.material (tenant_id, platform.normalized_text(description));

-- A material change (ADR-0082 §9): numbered MC and eight digits by the
-- context, once in the tenant; kind create, update, block (applied when
-- requested) or unblock; payload what it asks for (for an update the
-- fields given, with the plants' companies and the companies whose
-- plants it replaces); possible_duplicates what a create found, an array.
-- Applied once, with applied_at; one pending change per material.
CREATE TABLE materials.material_change (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  number               text NOT NULL CONSTRAINT material_change_number_check CHECK (number ~ '^MC[0-9]{8}$'),
  kind                 text NOT NULL CONSTRAINT material_change_kind_check CHECK (kind IN ('create', 'update', 'block', 'unblock')),
  material_id          uuid NOT NULL,
  payload              jsonb NOT NULL CONSTRAINT material_change_payload_object CHECK (jsonb_typeof(payload) = 'object'),
  reason               text NOT NULL DEFAULT '' CONSTRAINT material_change_reason_check CHECK (length(reason) <= 500),
  possible_duplicates  jsonb NOT NULL DEFAULT '[]' CONSTRAINT material_change_possible_duplicates_array CHECK (jsonb_typeof(possible_duplicates) = 'array'),
  status               text NOT NULL CONSTRAINT material_change_status_check CHECK (status IN ('pending', 'applied', 'rejected', 'cancelled')),
  workflow_request_id  uuid,              -- workflow context: its approval request
  requested_by         uuid NOT NULL,
  applied_at           timestamptz,
  version              integer NOT NULL DEFAULT 1 CONSTRAINT material_change_version_check CHECK (version >= 1),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT material_change_number_once UNIQUE (tenant_id, number),
  CONSTRAINT material_change_material_fkey FOREIGN KEY (tenant_id, material_id) REFERENCES materials.material (tenant_id, id),
  CONSTRAINT material_change_applied_check CHECK ((status = 'applied') = (applied_at IS NOT NULL))
);
-- One pending change per material: a second waits for the first's decision.
CREATE UNIQUE INDEX material_change_one_pending ON materials.material_change (tenant_id, material_id) WHERE status = 'pending';
-- A material's changes, newest first, and the tenant's.
CREATE INDEX material_change_by_material ON materials.material_change (tenant_id, material_id, created_at, id);
CREATE INDEX material_change_by_created ON materials.material_change (tenant_id, created_at, id);
CREATE TRIGGER material_change_touch BEFORE UPDATE ON materials.material_change FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('materials.material_change'::regclass);

-- Approving material changes: an approval permission, which agents never
-- hold (ADR-0082 §9).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('materials.change.approve', 'Approve material changes', true);
