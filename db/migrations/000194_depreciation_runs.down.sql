DELETE FROM identity.sod_rule WHERE permission_a = 'assets.depreciation.run' OR permission_b = 'assets.depreciation.run';
DELETE FROM identity.role_permission WHERE permission_code = 'assets.depreciation.run';
DELETE FROM ref.permission WHERE code = 'assets.depreciation.run';
DROP TABLE assets.depreciation_batch;
DROP FUNCTION assets.check_depreciation_batch();
DROP TABLE assets.depreciation_run;
DROP FUNCTION assets.check_depreciation_run();
