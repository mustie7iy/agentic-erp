DROP TABLE ledger.default_assignment;
DROP TABLE ledger.company_controlling;
DROP FUNCTION ledger.check_company_controlling();
ALTER TABLE ledger.cost_center_period DROP CONSTRAINT cost_center_period_category;
DROP TABLE ledger.cost_center_category;
ALTER TABLE ledger.gl_account DROP COLUMN cost_element;
