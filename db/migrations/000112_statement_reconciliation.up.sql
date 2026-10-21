-- Reconciling bank statements, and incoming payments (T49h; ADR-0047;
-- 03-data-model §3.10). A statement becomes reconciled, once and never
-- again by itself, when every line is posted or excluded and its closing
-- balance, less the lines excluded so far, is the ledger's balance of the
-- house bank's main bank account on its closing date; when and by whom is
-- recorded. An incoming payment a person matches by hand names neither a
-- payment nor a posting rule: it posts to the house bank's incoming
-- payments clearing account until accounts receivable (Phase 3) clears it.

ALTER TABLE payments.bank_statement
  ADD COLUMN reconciled_at timestamptz,
  ADD COLUMN reconciled_by uuid,
  -- A reconciled statement says when and by whom, and only it does.
  ADD CONSTRAINT bank_statement_reconciled CHECK ((status = 'reconciled') = (reconciled_at IS NOT NULL)
    AND (reconciled_at IS NULL) = (reconciled_by IS NULL));

-- A target is a payment (with the bank file of a batch booking), a
-- posting rule, or the incoming payments clearing account: one of them.
ALTER TABLE payments.statement_match_target
  ADD COLUMN incoming boolean NOT NULL DEFAULT false,
  DROP CONSTRAINT statement_match_target_one,
  ADD CONSTRAINT statement_match_target_one CHECK (num_nonnulls(payment_id, posting_rule) + incoming::int = 1
    AND (bank_file_id IS NULL OR payment_id IS NOT NULL));

-- The matches of a line, live and rejected: the reconciliation report
-- reads the outcome of the latest proposal a person decided, and the
-- statement's matches are listed by line.
CREATE INDEX statement_match_line ON payments.statement_match (tenant_id, statement_id, line_no);
