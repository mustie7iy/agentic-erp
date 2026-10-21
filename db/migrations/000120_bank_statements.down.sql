DELETE FROM identity.sod_rule WHERE permission_a IN ('payments.statement.read', 'payments.statement.import')
  OR permission_b IN ('payments.statement.read', 'payments.statement.import');
DELETE FROM identity.role_permission WHERE permission_code IN ('payments.statement.read', 'payments.statement.import');
DELETE FROM ref.permission WHERE code IN ('payments.statement.read', 'payments.statement.import');
DROP TABLE payments.statement_match;
DROP TABLE payments.bank_statement_line;
DROP TABLE payments.bank_statement;
