-- migrate:no-transaction
-- An org unit's code stays unique per legal entity and kind for every kind
-- but storage locations (tracker leaf "Storage location codes unique per
-- plant", ADR-0082 As built): what the key of 000002 held for all kinds,
-- built concurrently (06 §6.4) before 000414 drops that key.
CREATE UNIQUE INDEX CONCURRENTLY org_unit_code_once ON tenancy.org_unit (tenant_id, legal_entity_id, kind, code)
  WHERE kind <> 'storage_location';
