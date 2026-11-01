DROP TRIGGER parked_entry_keep_limit ON ledger.parked_entry;
DROP FUNCTION ledger.keep_parked_entry_limit();
ALTER TABLE ledger.parked_entry DROP COLUMN limit_currency, DROP COLUMN limit_amount, DROP COLUMN limit_debits_currency,
  DROP COLUMN limit_debits, DROP COLUMN limit_kind;
