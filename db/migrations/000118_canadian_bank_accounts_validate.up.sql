-- Validates what 000117 added NOT VALID (06 §6.4): every account before it
-- is an IBAN or domestic one without institution or transit numbers.
ALTER TABLE partner.bank_account VALIDATE CONSTRAINT bank_account_account_scheme_check;
ALTER TABLE partner.bank_account VALIDATE CONSTRAINT bank_account_ca_routing;
ALTER TABLE payments.house_bank VALIDATE CONSTRAINT house_bank_ca_routing;
