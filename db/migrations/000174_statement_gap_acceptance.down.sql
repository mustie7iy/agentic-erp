-- Without its acceptance a gap stands again: a statement accepted, not
-- reconciled since, and still not opening at its previous statement's
-- closing balance goes back to gap (one whose missing statement arrived
-- since stays imported).
DELETE FROM identity.sod_rule WHERE permission_a = 'payments.statement.gap.accept' OR permission_b = 'payments.statement.gap.accept';
DELETE FROM identity.role_permission WHERE permission_code = 'payments.statement.gap.accept';
DELETE FROM ref.permission WHERE code = 'payments.statement.gap.accept';
ALTER TABLE payments.bank_statement DROP CONSTRAINT bank_statement_gap_accepted;
UPDATE payments.bank_statement s SET status = 'gap' WHERE s.gap_accepted_at IS NOT NULL AND s.status = 'imported'
  AND s.opening_balance <> (SELECT p.closing_balance FROM payments.bank_statement p WHERE p.tenant_id = s.tenant_id AND p.id = s.previous_statement_id);
ALTER TABLE payments.bank_statement
  DROP COLUMN gap_accepted_at,
  DROP COLUMN gap_accepted_by,
  DROP COLUMN gap_reason;
