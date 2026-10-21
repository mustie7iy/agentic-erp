DROP INDEX ledger.journal_line_tax_code;
DROP TABLE ledger.tax_rate;
DROP FUNCTION ledger.check_tax_rate();
DROP FUNCTION ledger.tax_code_posted(uuid, uuid, text, date, date);
DROP TABLE ledger.tax_component;
DROP FUNCTION ledger.tax_component_account_history();
DROP TABLE ledger.tax_code_definition;
