-- Fails once a posting has copies in several ledgers: they share a number.
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_tenant_id_fiscal_year_legal_entity_id_documen_key
  UNIQUE (tenant_id, fiscal_year, legal_entity_id, document_type, document_number);
