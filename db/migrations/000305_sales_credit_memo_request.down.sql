DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.credit_memo_request.read', 'sales.credit_memo_request.write', 'sales.credit_memo_request.approve')
  OR permission_b IN ('sales.credit_memo_request.read', 'sales.credit_memo_request.write', 'sales.credit_memo_request.approve');
DELETE FROM identity.role_permission
  WHERE permission_code IN ('sales.credit_memo_request.read', 'sales.credit_memo_request.write', 'sales.credit_memo_request.approve');
DELETE FROM ref.permission WHERE code IN ('sales.credit_memo_request.read', 'sales.credit_memo_request.write', 'sales.credit_memo_request.approve');
DROP TABLE sales.credit_memo_request_item;
DROP TABLE sales.credit_memo_request;
DROP FUNCTION sales.check_credit_memo_request_item();
DROP FUNCTION sales.check_credit_memo_request_change();
DROP FUNCTION sales.check_credit_memo_request();
