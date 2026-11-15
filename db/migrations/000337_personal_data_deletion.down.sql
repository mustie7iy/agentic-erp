DELETE FROM identity.sod_rule WHERE permission_a = 'records.privacy.delete' OR permission_b = 'records.privacy.delete';
DELETE FROM identity.role_permission WHERE permission_code = 'records.privacy.delete';
DELETE FROM ref.permission WHERE code = 'records.privacy.delete';
DROP TABLE records.deletion_run_subject;
DROP FUNCTION records.check_deletion_subject_change();
DROP TABLE records.deletion_run;
DROP FUNCTION records.check_deletion_run_change();

REVOKE UPDATE (holder_name, account_last4, bic, institution_number, transit_number) ON partner.bank_account FROM erp_app;
REVOKE UPDATE (line1, line2, city, postal_code, region) ON partner.address FROM erp_app;
DROP TRIGGER bank_account_end ON partner.bank_account;
CREATE TRIGGER bank_account_end BEFORE UPDATE ON partner.bank_account FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
DROP TRIGGER address_end ON partner.address;
CREATE TRIGGER address_end BEFORE UPDATE ON partner.address FOR EACH ROW EXECUTE FUNCTION platform.end_validity();
DROP TRIGGER bank_account_personal_fixed ON partner.bank_account;
DROP TRIGGER address_personal_fixed ON partner.address;
DROP FUNCTION partner.check_personal_columns();
DROP FUNCTION partner.bank_account_blanked(partner.bank_account, partner.bank_account);
DROP FUNCTION partner.address_blanked(partner.address, partner.address);

DROP TRIGGER business_partner_personal_data_deleted_fixed ON partner.business_partner;
DROP FUNCTION partner.check_personal_data_deleted();
ALTER TABLE partner.business_partner DROP CONSTRAINT business_partner_personal_data_deleted_check,
  DROP COLUMN personal_data_deleted_at;
