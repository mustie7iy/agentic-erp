-- Descriptions by language (S1a2; ADR-0082 As built S1a2; 03-data-model
-- §3.34): a material's description is its default one, in the language
-- description_language names, and the material may have one further
-- description per other language (SAP MAKT, from memory). A language is a
-- BCP 47 tag of a language with an optional script and region, in their
-- canonical case (the application checks the subtags are known).
--
-- Expand only (06 §6.4): the column comes with the constant default 'en'
-- (no table rewrite), the language every material created before was
-- described in, and which the previous version's inserts, naming no
-- language, take; its check is added NOT VALID and validated by 000470.
-- The previous version neither reads nor writes the new table. Nothing is
-- contracted: description stays the default description's text.
ALTER TABLE materials.material
  ADD COLUMN description_language text NOT NULL DEFAULT 'en',
  ADD CONSTRAINT material_description_language_check
    CHECK (description_language ~ '^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$') NOT VALID;

-- The default description's language stays as created: a further
-- description in it would otherwise appear (material_description_not_default
-- reads it). Its own function, so material_fixed stays as 000435 left it.
CREATE FUNCTION materials.material_language_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.description_language <> OLD.description_language THEN
    RAISE EXCEPTION 'a material''s default description language does not change'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_language_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER material_language_fixed BEFORE UPDATE ON materials.material FOR EACH ROW EXECUTE FUNCTION materials.material_language_fixed();

-- A further description of a material in a language other than its
-- default description's, once per language. Nothing is deleted (000011): a
-- description a change takes off is marked removed, and given back, with
-- its text, by clearing the mark, as a unit is.
CREATE TABLE materials.material_description (
  tenant_id    uuid NOT NULL,
  material_id  uuid NOT NULL,
  language     text NOT NULL CONSTRAINT material_description_tag_check
    CHECK (language ~ '^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$'),
  description  text NOT NULL CONSTRAINT material_description_text_check
    CHECK (length(description) BETWEEN 1 AND 200 AND length(trim(description)) > 0),
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  removed_at   timestamptz,
  removed_by   uuid,
  CONSTRAINT material_description_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
  PRIMARY KEY (tenant_id, material_id, language),
  CONSTRAINT material_description_material_fkey FOREIGN KEY (tenant_id, material_id) REFERENCES materials.material (tenant_id, id)
);
-- A description in the default's language is the default description
-- itself, so no row, removed or not, is in it; the default's language does
-- not change (material_language_fixed), so the check holds without a lock
-- on the material.
CREATE FUNCTION materials.material_description_not_default() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM materials.material m
      WHERE m.tenant_id = NEW.tenant_id AND m.id = NEW.material_id AND m.description_language = NEW.language) THEN
    RAISE EXCEPTION 'the default description''s language has no further description'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_description_not_default';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER material_description_not_default BEFORE INSERT OR UPDATE ON materials.material_description
  FOR EACH ROW EXECUTE FUNCTION materials.material_description_not_default();
SELECT platform.secure_table('materials.material_description'::regclass);
-- A changed text is updated in place, a removed description marked.
GRANT UPDATE (description, removed_at, removed_by) ON materials.material_description TO erp_app;
