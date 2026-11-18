DELETE FROM identity.sod_rule WHERE permission_a = 'collections.worklist.write' OR permission_b = 'collections.worklist.write';
DELETE FROM identity.role_permission WHERE permission_code = 'collections.worklist.write';
DELETE FROM ref.permission WHERE code = 'collections.worklist.write';
ALTER TABLE collections.dunning_run DROP COLUMN promised_items;
DROP TABLE collections.promise_to_pay_item;
DROP TABLE collections.promise_to_pay;
