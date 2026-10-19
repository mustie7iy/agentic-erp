DELETE FROM identity.sod_rule WHERE permission_a = 'payables.down_payment.request' OR permission_b = 'payables.down_payment.request';
DELETE FROM identity.role_permission WHERE permission_code = 'payables.down_payment.request';
DELETE FROM ref.permission WHERE code = 'payables.down_payment.request';
DROP TABLE payables.down_payment_request;
DROP FUNCTION payables.check_down_payment_request_change();
