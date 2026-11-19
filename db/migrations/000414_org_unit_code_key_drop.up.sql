-- The org unit code key of 000002 (unique per legal entity and kind) gives
-- way to org_unit_code_once and storage_location_code_once (000412,
-- 000413), built already: a storage location's code is unique under its
-- plant. The version before creates storage locations a company key held
-- unique, which the plant key holds as well.
ALTER TABLE tenancy.org_unit DROP CONSTRAINT org_unit_tenant_id_legal_entity_id_kind_code_key;
