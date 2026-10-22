DELETE FROM identity.sod_rule WHERE permission_a IN ('cash.memo.read', 'cash.memo.write', 'cash.memo.propose')
  OR permission_b IN ('cash.memo.read', 'cash.memo.write', 'cash.memo.propose');
DELETE FROM identity.role_permission WHERE permission_code IN ('cash.memo.read', 'cash.memo.write', 'cash.memo.propose');
DELETE FROM ref.permission WHERE code IN ('cash.memo.read', 'cash.memo.write', 'cash.memo.propose');
DROP TABLE cash.memo_record;
DROP FUNCTION cash.check_memo_item();
REVOKE ALL ON SCHEMA cash FROM erp_relay;
DROP TABLE cash.inbox;
DROP TABLE cash.outbox;
