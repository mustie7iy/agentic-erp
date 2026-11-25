-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000480's state by adding it back NOT VALID.
ALTER TABLE materials.material
  DROP CONSTRAINT material_description_language_check,
  ADD CONSTRAINT material_description_language_check
    CHECK (description_language ~ '^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$') NOT VALID;
