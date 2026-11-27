-- A reconciled statement keeps what its lines add to the reconciliation
-- figures (T49, ADR-0047 As built finfix), so a check reads the lines of
-- the statements not reconciled and not a bank's whole history: once
-- reconciled, a statement's lines are decided for good (posted or
-- excluded, its decisions refused with 409 payments.statement_reconciled),
-- so what it adds never changes. reconciled_excluded is the sum of its
-- excluded lines, what it adds to every later date's excluded figure;
-- reconciled_posted_until the latest posting date of its posted lines'
-- matches, null without one: its lines add to a date's posted_later only
-- before that date, so a read of later dates leaves them unread.
-- Expand only: a statement a server of the release before reconciles
-- during the rolling upgrade has neither, and the figures read its lines
-- as before (payments.figures), so no check requires them.
ALTER TABLE payments.bank_statement
  ADD COLUMN reconciled_excluded bigint,
  ADD COLUMN reconciled_posted_until date;
-- Tenant rows: under a role that does not bypass row-level security this
-- fails rather than backfill nothing, as 000430.
-- Its touch trigger set aside, so a statement's updated_at stays when it
-- was last changed.
SET LOCAL row_security = off;
ALTER TABLE payments.bank_statement DISABLE TRIGGER bank_statement_touch;
UPDATE payments.bank_statement s SET
  reconciled_excluded = (SELECT coalesce(sum(l.amount), 0) FROM payments.bank_statement_line l
    WHERE l.tenant_id = s.tenant_id AND l.statement_id = s.id AND l.status = 'excluded'),
  reconciled_posted_until = (SELECT max(m.posting_date) FROM payments.bank_statement_line l
    JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no AND m.status <> 'rejected'
    WHERE l.tenant_id = s.tenant_id AND l.statement_id = s.id AND l.status = 'posted')
WHERE s.status = 'reconciled';
ALTER TABLE payments.bank_statement ENABLE TRIGGER bank_statement_touch;
