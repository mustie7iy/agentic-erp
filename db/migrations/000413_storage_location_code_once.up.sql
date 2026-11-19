-- migrate:no-transaction
-- A storage location's code is unique under its plant (its parent), as SAP
-- keys LGORT within its WERKS (T001L, from memory), not within its
-- company: two plants of a company may each have a storage location 0001
-- (ADR-0082 As built, storage location codes). Storage locations without
-- a parent stay unique in their legal entity (NULLS NOT DISTINCT), as
-- before. Built concurrently (06 §6.4) before 000414 drops the company key.
CREATE UNIQUE INDEX CONCURRENTLY storage_location_code_once ON tenancy.org_unit (tenant_id, legal_entity_id, parent_id, code)
  NULLS NOT DISTINCT WHERE kind = 'storage_location';
