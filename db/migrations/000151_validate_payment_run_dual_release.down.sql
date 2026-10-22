-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000150's state by adding each one back NOT VALID.
ALTER TABLE payments.payment_run
  DROP CONSTRAINT payment_run_release_sod,
  ADD CONSTRAINT payment_run_release_sod
    CHECK (release_approved_by IS NULL OR (release_approved_by <> proposed_by
      AND (status NOT IN ('releasing', 'released') OR decided_by <> release_approved_by))) NOT VALID,
  DROP CONSTRAINT payment_run_release_approved,
  ADD CONSTRAINT payment_run_release_approved
    CHECK (coalesce(release_approval = 'approved', false) = (release_approved_by IS NOT NULL)
      AND (release_approved_by IS NULL) = (release_approved_at IS NULL)) NOT VALID,
  DROP CONSTRAINT payment_run_release_request,
  ADD CONSTRAINT payment_run_release_request
    CHECK ((release_request_id IS NULL) = (release_approval IS NULL) AND (release_request_id IS NULL) = (release_request_total IS NULL)) NOT VALID,
  DROP CONSTRAINT payment_run_release_approval_check,
  ADD CONSTRAINT payment_run_release_approval_check
    CHECK (release_approval IN ('pending', 'approved', 'rejected', 'withdrawn')) NOT VALID;
