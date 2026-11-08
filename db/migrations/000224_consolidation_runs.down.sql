DELETE FROM identity.sod_rule WHERE permission_a = 'consolidation.run.manage' OR permission_b = 'consolidation.run.manage';
DELETE FROM identity.role_permission WHERE permission_code = 'consolidation.run.manage';
DELETE FROM ref.permission WHERE code = 'consolidation.run.manage';
DROP SCHEMA consolidation CASCADE;
