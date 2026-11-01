-- No earlier migration grants erp_worker these columns, so dropping the
-- table takes its grant with it; the parked entry's kind check is 000052's.
DROP TRIGGER parked_entry_keep_template ON ledger.parked_entry;
DROP FUNCTION ledger.keep_parked_entry_template();
DROP INDEX ledger.parked_entry_by_owner;
DROP INDEX ledger.parked_entry_by_template;
DROP INDEX ledger.parked_entry_occurrence_once;
ALTER TABLE ledger.parked_entry
  DROP CONSTRAINT parked_entry_occurrence,
  DROP CONSTRAINT parked_entry_template_fkey,
  DROP CONSTRAINT parked_entry_created_by_kind,
  ADD CONSTRAINT parked_entry_created_by_kind_check CHECK (created_by_kind IN ('human', 'agent', 'service')),
  DROP COLUMN occurrence_date,
  DROP COLUMN template_id;
DROP TABLE ledger.entry_template_line;
DROP TABLE ledger.entry_template;
DROP FUNCTION ledger.check_entry_template_change();
