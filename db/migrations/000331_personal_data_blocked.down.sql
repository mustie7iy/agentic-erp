DELETE FROM identity.sod_rule WHERE permission_a IN ('partner.privacy.block', 'partner.privacy.read_blocked')
  OR permission_b IN ('partner.privacy.block', 'partner.privacy.read_blocked');
DELETE FROM identity.role_permission WHERE permission_code IN ('partner.privacy.block', 'partner.privacy.read_blocked');
DELETE FROM ref.permission WHERE code IN ('partner.privacy.block', 'partner.privacy.read_blocked');
ALTER TABLE partner.business_partner DROP CONSTRAINT business_partner_personal_data_blocked_check,
  DROP COLUMN personal_data_blocked_at;
