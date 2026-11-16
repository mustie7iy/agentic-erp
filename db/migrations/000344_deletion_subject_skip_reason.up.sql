-- Why a deletion run skipped a subject (ADR-0079 §12, review:batch74): the
-- partner context's reason, `already_deleted`, `block_lifted`,
-- `blocked_again` or `open_item`, kept with the subject the deleter
-- settles. Expand-only: a subject skipped before this migration keeps no
-- reason, so the column is null on every subject not skipped and may be
-- null on one skipped before.
ALTER TABLE records.deletion_run_subject
  ADD COLUMN skip_reason text
    CONSTRAINT deletion_run_subject_skip_reason_check CHECK (skip_reason IN ('already_deleted', 'block_lifted', 'blocked_again', 'open_item')),
  ADD CONSTRAINT deletion_run_subject_skip_reason_skipped_check CHECK (skip_reason IS NULL OR status = 'skipped');
GRANT UPDATE (skip_reason) ON records.deletion_run_subject TO erp_app;
