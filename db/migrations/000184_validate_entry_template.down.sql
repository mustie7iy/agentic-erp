-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000183's state by adding each one back NOT VALID.
ALTER TABLE ledger.parked_entry
  DROP CONSTRAINT parked_entry_occurrence,
  ADD CONSTRAINT parked_entry_occurrence CHECK ((occurrence_date IS NULL OR template_id IS NOT NULL)
      AND (created_by_kind = 'system') = (occurrence_date IS NOT NULL)
      AND (occurrence_date IS NULL OR owner_id IS NOT NULL)) NOT VALID,
  DROP CONSTRAINT parked_entry_template_fkey,
  ADD CONSTRAINT parked_entry_template_fkey FOREIGN KEY (tenant_id, template_id) REFERENCES ledger.entry_template (tenant_id, id) NOT VALID,
  DROP CONSTRAINT parked_entry_created_by_kind,
  ADD CONSTRAINT parked_entry_created_by_kind CHECK (created_by_kind IN ('human', 'agent', 'service', 'system')) NOT VALID;
