-- An approved exception counts for the invoice as it was approved (F16,
-- ADR-0008): the invoice records the version the approval left it at, and
-- posting on the approval alone requires that version to be the invoice's
-- current one. Before, a linked approved request let the invoice post
-- whatever happened to it after the approval; this release's edit unlinks
-- the request, but the previous release's edit does not, so during the
-- rollout an invoice edited on an old replica kept its approval.
--
-- Expand step (06-engineering-process §6.4): the column is nullable, so the
-- previous release, which approves without it, keeps running during the
-- rollout; the check is added NOT VALID (existing rows are all NULL, and
-- new and updated rows are checked). No backfill: nothing records which
-- version an exception was approved at, and the invoice may have been
-- edited since, so its current version would be a guess that could
-- authorise an edited invoice. A NULL version is "not this version": an
-- invoice approved before this migration, or by the previous release during
-- the rollout, is matched clean or approved again before it posts (409
-- payables.match_required), failing closed. There is no rule tying the
-- column to workflow_request_id: the previous release unlinks requests
-- without clearing it, and a stale version is always below the invoice's,
-- as every change moves the version on. Grants and row-level security are
-- the table's.
ALTER TABLE payables.supplier_invoice ADD COLUMN exception_version integer;
ALTER TABLE payables.supplier_invoice ADD CONSTRAINT supplier_invoice_exception_version_valid
  CHECK (exception_version > 0 AND exception_version <= version) NOT VALID;
COMMENT ON COLUMN payables.supplier_invoice.exception_version IS
  'The invoice version its approved exception request (workflow_request_id) left it at; the approval lets only that version post. NULL (approvals before 000041): not any version';
