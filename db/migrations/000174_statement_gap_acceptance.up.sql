-- A person accepts a bank statement's gap without the missing statement
-- (ADR-0047 Continuity; 03-data-model §3.10): the statement in gap moves
-- to imported and the bank's lines post again. Who accepted it, when and
-- why is kept on the statement while it stands; a statement whose
-- previous statement changes later (the missing one arrives) is compared
-- again, and a gap found then is a new one, so the acceptance goes. The
-- check is added NOT VALID and validated in 000175 (06 §6.4).
ALTER TABLE payments.bank_statement
  ADD COLUMN gap_accepted_at timestamptz,
  ADD COLUMN gap_accepted_by uuid,
  ADD COLUMN gap_reason text,
  -- An acceptance names when, who and why, all or none; a statement in
  -- gap has none, and only a statement with a previous one was in gap.
  ADD CONSTRAINT bank_statement_gap_accepted CHECK (num_nulls(gap_accepted_at, gap_accepted_by, gap_reason) IN (0, 3)
    AND (gap_reason IS NULL OR (gap_reason <> '' AND status <> 'gap' AND previous_statement_id IS NOT NULL))) NOT VALID;

-- Accepting a gap: a person's decision (x-agent: false), not an approval.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payments.statement.gap.accept', 'Accept a bank statement''s gap without the missing statement, so the bank''s lines post again', false);
