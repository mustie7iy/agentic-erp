DROP TRIGGER parked_entry_ledgers_fixed ON ledger.parked_entry;
DROP FUNCTION ledger.check_parked_entry_ledgers();
ALTER TABLE ledger.parked_entry DROP COLUMN ledger_specific, DROP COLUMN ledger_group_id;
DROP TRIGGER journal_entry_ledger ON ledger.journal_entry;
DROP FUNCTION ledger.check_entry_ledger();
DROP FUNCTION ledger.company_keeps(uuid, uuid);
ALTER TABLE ledger.journal_entry DROP COLUMN ledger_group_id, DROP COLUMN posting_group_id;
