-- migrate:no-transaction
-- A line's matches, live and rejected (T49h; ADR-0047): the reconciliation
-- report reads the latest proposal a person decided by them, and a
-- statement's matches are listed by line. Built concurrently (06 §6.4); no
-- IF NOT EXISTS, so an invalid index a failed build left is dropped by hand.
CREATE INDEX CONCURRENTLY statement_match_line ON payments.statement_match (tenant_id, statement_id, line_no);
