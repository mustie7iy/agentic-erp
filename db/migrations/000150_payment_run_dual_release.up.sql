-- Two-person release of large payment runs (ADR-0038, product owner
-- 2026-10-22). A run whose total, in its company's currency at the run
-- date's rate, is above the company's release_dual_limit is released only
-- after a second person approved its release (workflow context, object
-- type payment_run_release). The run keeps the latest such request, its
-- outcome, the total it was asked for and who approved it, so :release
-- reads the approval from the run it locks.
-- The columns are nullable without a default and the checks NOT VALID, so
-- nothing rewrites or scans the table under its lock (06 §6.4); 000151
-- validates them.
ALTER TABLE payments.payment_run
  ADD COLUMN release_request_id uuid,          -- workflow context: the latest request for the release's approval
  ADD COLUMN release_approval text,            -- its outcome as the run knows it
  ADD COLUMN release_request_total bigint,     -- the run's total when it was asked for, minor units of the run's currency
  ADD COLUMN release_approved_by uuid,         -- the second person
  ADD COLUMN release_approved_at timestamptz,
  ADD CONSTRAINT payment_run_release_approval_check
    CHECK (release_approval IN ('pending', 'approved', 'rejected', 'withdrawn')) NOT VALID,
  ADD CONSTRAINT payment_run_release_request
    CHECK ((release_request_id IS NULL) = (release_approval IS NULL) AND (release_request_id IS NULL) = (release_request_total IS NULL)) NOT VALID,
  ADD CONSTRAINT payment_run_release_approved
    CHECK (coalesce(release_approval = 'approved', false) = (release_approved_by IS NOT NULL)
      AND (release_approved_by IS NULL) = (release_approved_at IS NULL)) NOT VALID,
  -- Segregation of duties: the second person is neither the proposer nor
  -- the releaser.
  ADD CONSTRAINT payment_run_release_sod
    CHECK (release_approved_by IS NULL OR (release_approved_by <> proposed_by
      AND (status NOT IN ('releasing', 'released') OR decided_by <> release_approved_by))) NOT VALID;
