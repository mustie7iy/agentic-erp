DROP TABLE ledger.revaluation_item;
DROP TABLE ledger.revaluation_run;
DROP FUNCTION ledger.check_revaluation_run();
DROP TRIGGER company_account_revalue_foreign_currency ON ledger.company_account;
DROP FUNCTION ledger.check_revalue_foreign_currency();
DROP TABLE ledger.fx_valuation_account;
DROP FUNCTION ledger.check_fx_valuation_account();
DROP TABLE ledger.fx_valuation;
ALTER TABLE ledger.company_account DROP COLUMN revalue_foreign_currency;
