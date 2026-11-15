DELETE FROM identity.sod_rule WHERE permission_a IN ('payables.expense.read', 'payables.expense.write', 'payables.expense.post')
  OR permission_b IN ('payables.expense.read', 'payables.expense.write', 'payables.expense.post');
DELETE FROM identity.role_permission WHERE permission_code IN ('payables.expense.read', 'payables.expense.write', 'payables.expense.post');
DELETE FROM ref.permission WHERE code IN ('payables.expense.read', 'payables.expense.write', 'payables.expense.post');
DROP TABLE payables.expense_report_line;
DROP TABLE payables.expense_report;
DROP FUNCTION payables.check_expense_report_change();
