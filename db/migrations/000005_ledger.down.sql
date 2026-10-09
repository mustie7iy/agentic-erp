DROP VIEW IF EXISTS ledger.open_item;
DROP TABLE IF EXISTS ledger.inbox, ledger.outbox, ledger.period_balance, ledger.clearing_item, ledger.clearing, ledger.reversal,
  ledger.journal_line, ledger.journal_entry, ledger.exchange_rate, ledger.tax_code, ledger.account_determination,
  ledger.company_account, ledger.gl_account, ledger.chart_of_accounts, ledger.period_status, ledger.fiscal_period,
  ledger.fiscal_calendar, ledger.ledger CASCADE;
DROP FUNCTION IF EXISTS ledger.check_entry_period(), ledger.check_line(), ledger.check_entry_balanced();
