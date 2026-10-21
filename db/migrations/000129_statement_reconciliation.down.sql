-- An incoming payment's match has no other target to fall back on, so the
-- migration stops rather than leave matches that clear nothing.
DO $$ BEGIN
  IF EXISTS (SELECT FROM payments.statement_match_target WHERE incoming) THEN
    RAISE EXCEPTION 'payments.statement_match_target has incoming payments; 000129 is undone only without them';
  END IF;
END $$;
ALTER TABLE payments.statement_match_target
  DROP CONSTRAINT statement_match_target_one,
  DROP COLUMN incoming,
  ADD CONSTRAINT statement_match_target_one CHECK (num_nonnulls(payment_id, posting_rule) = 1 AND (bank_file_id IS NULL OR payment_id IS NOT NULL));
ALTER TABLE payments.bank_statement DROP CONSTRAINT bank_statement_reconciled;
UPDATE payments.bank_statement SET status = 'imported' WHERE status = 'reconciled';
ALTER TABLE payments.bank_statement
  DROP COLUMN reconciled_at,
  DROP COLUMN reconciled_by;
