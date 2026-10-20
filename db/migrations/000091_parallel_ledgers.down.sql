DROP TABLE ledger.ledger_group, ledger.ledger_group_member, ledger.company_ledger CASCADE;
DROP FUNCTION ledger.check_group_representative();
DROP FUNCTION ledger.check_ledger_group_update();
DROP FUNCTION ledger.check_company_has_leading();
REVOKE UPDATE (name, version) ON ledger.ledger FROM erp_app;
ALTER TABLE ledger.ledger DROP CONSTRAINT ledger_role_key;
ALTER TABLE ledger.ledger DROP CONSTRAINT ledger_name_check;
ALTER TABLE ledger.ledger DROP COLUMN version;
ALTER TABLE ledger.ledger DROP CONSTRAINT ledger_leading_is_0l;
