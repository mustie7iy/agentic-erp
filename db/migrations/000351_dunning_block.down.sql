DELETE FROM identity.sod_rule WHERE permission_a = 'collections.dunning.block' OR permission_b = 'collections.dunning.block';
DELETE FROM identity.role_permission WHERE permission_code = 'collections.dunning.block';
DELETE FROM ref.permission WHERE code = 'collections.dunning.block';
ALTER TABLE collections.dunning_run DROP COLUMN blocked_items;
DROP TABLE collections.dunning_block;
DROP FUNCTION collections.check_dunning_block_change();
