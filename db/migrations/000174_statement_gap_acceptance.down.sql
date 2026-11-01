-- Without its acceptance a gap stands again: a statement accepted and not
-- reconciled since goes back to gap.
DELETE FROM identity.role_permission WHERE permission_code = 'payments.statement.gap.accept';
DELETE FROM ref.permission WHERE code = 'payments.statement.gap.accept';
ALTER TABLE payments.bank_statement DROP CONSTRAINT bank_statement_gap_accepted;
UPDATE payments.bank_statement SET status = 'gap' WHERE gap_accepted_at IS NOT NULL AND status = 'imported';
ALTER TABLE payments.bank_statement
  DROP COLUMN gap_accepted_at,
  DROP COLUMN gap_accepted_by,
  DROP COLUMN gap_reason;
