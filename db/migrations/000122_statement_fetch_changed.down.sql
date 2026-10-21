-- A change recorded is still a file imported and not archived, which is
-- what 000121 knew it as.
UPDATE payments.statement_fetch SET status = 'imported', reason = NULL WHERE status = 'changed_after_import';
ALTER TABLE payments.statement_fetch DROP CONSTRAINT statement_fetch_status_check,
  ADD CONSTRAINT statement_fetch_status_check CHECK (status IN ('imported', 'archived', 'refused')),
  DROP CONSTRAINT statement_fetch_reason,
  ADD CONSTRAINT statement_fetch_reason CHECK ((status = 'refused') = (reason IS NOT NULL));
