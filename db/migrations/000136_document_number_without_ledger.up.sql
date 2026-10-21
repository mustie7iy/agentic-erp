-- The document number was unique per legal entity, year and type; since
-- 000134 it is unique per ledger too, so a posting's copies share it
-- (T52b). The old constraint would refuse every copy, so it goes now rather
-- than in T52e: dropping a uniqueness the previous release never relies on
-- (its numbers come from the number range, never from a conflict) is safe
-- during a rolling deploy, and the index of 000134 still holds every ledger
-- to unique numbers.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_tenant_id_fiscal_year_legal_entity_id_documen_key;
