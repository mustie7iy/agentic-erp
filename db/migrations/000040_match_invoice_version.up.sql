-- A match counts for the invoice as it was matched (F13, ADR-0008): the
-- result records the invoice version it leaves the invoice at, and posting
-- requires the latest match to be clean and of the invoice's current
-- version. Before, the latest clean match let an invoice post after its
-- amounts, items or order were edited.
--
-- Expand step (06-engineering-process §6.4): the column is nullable, so the
-- previous release, which records matches without it, keeps running during
-- the rollout; the check is added NOT VALID (existing rows are all NULL, and
-- new rows are checked). No backfill: nothing records which version an
-- existing match evaluated, and an invoice may have been edited since, so
-- the invoice's current version would be a guess that could authorise an
-- edited invoice. A NULL version is "not this version": an invoice whose
-- latest match predates this migration, or was recorded by the previous
-- release during the rollout, is matched again before it posts (409
-- payables.match_required), failing closed. Rows stay insert-only
-- (match_result_immutable); grants and row-level security are the table's.
ALTER TABLE payables.match_result ADD COLUMN invoice_version integer;
ALTER TABLE payables.match_result ADD CONSTRAINT match_result_invoice_version_positive CHECK (invoice_version > 0) NOT VALID;
COMMENT ON COLUMN payables.match_result.invoice_version IS
  'The supplier invoice version the match left the invoice at; posting needs the latest match clean at the invoice''s current version. NULL (matches before 000040): not any version';
