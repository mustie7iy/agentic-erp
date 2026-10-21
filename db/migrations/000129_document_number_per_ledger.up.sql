-- migrate:partitioned-index
-- A posting's copies in the company's ledgers share its document number
-- (T52b, ADR-0045), so the number is unique per ledger. Built per
-- partition and attached (06 §6.4), writes going on meanwhile; 000131
-- then drops the constraint without the ledger.
CREATE UNIQUE INDEX journal_entry_document ON ledger.journal_entry (tenant_id, fiscal_year, legal_entity_id, ledger_id, document_type, document_number);
