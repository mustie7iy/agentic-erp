DROP TABLE materials.material_description;
DROP FUNCTION materials.material_description_not_default();
DROP TRIGGER material_language_fixed ON materials.material;
DROP FUNCTION materials.material_language_fixed();
ALTER TABLE materials.material DROP COLUMN description_language;
