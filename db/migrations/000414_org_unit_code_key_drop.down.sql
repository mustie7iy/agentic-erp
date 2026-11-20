-- Fails while a company has one storage location code under two plants,
-- which the previous schema cannot hold: org units are not deleted to make
-- room for it.
ALTER TABLE tenancy.org_unit ADD CONSTRAINT org_unit_tenant_id_legal_entity_id_kind_code_key UNIQUE (tenant_id, legal_entity_id, kind, code);
