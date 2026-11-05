DROP TRIGGER balance_projection_trading_partner ON ledger.balance_projection;
DROP FUNCTION ledger.project_trading_partner_balance();
DROP TABLE ledger.trading_partner_balance;
ALTER TABLE ledger.entry_template_line DROP COLUMN trading_partner_id;
ALTER TABLE ledger.parked_entry_line DROP COLUMN trading_partner_id;
-- Dropping the column drops its check on every partition.
ALTER TABLE ledger.journal_line DROP COLUMN trading_partner_id;
