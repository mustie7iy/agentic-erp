-- Period unlock (T32, 03-data-model §3.10). A locked period returns to
-- closed only through an approved period_unlock approval request. While the
-- request is pending the period names it in unlock_request_id; the column
-- is cleared when the request ends, so only a locked period can hold one.
-- The transition trigger of 000050 now lets locked -> closed through when
-- the request the period names is an approved unlock of this period, read
-- in the deciding transaction (approvals marks the request approved before
-- it calls the ledger back), so no code path can unlock without approval.
ALTER TABLE ledger.period_status ADD COLUMN unlock_request_id uuid; -- workflow context
ALTER TABLE ledger.period_status ADD CONSTRAINT period_status_unlock_only_locked
  CHECK (unlock_request_id IS NULL OR status = 'locked');

CREATE OR REPLACE FUNCTION ledger.check_period_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) IN (('open', 'closed'), ('closed', 'open'), ('closed', 'locked')) THEN
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) = ('locked', 'closed') AND EXISTS (
       SELECT 1 FROM workflow.approval_request r
        WHERE r.tenant_id = OLD.tenant_id AND r.id = OLD.unlock_request_id
          AND r.object_type = 'period_unlock' AND r.object_id = OLD.id AND r.status = 'approved') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'period %/% cannot go from % to %', OLD.fiscal_year, OLD.period, OLD.status, NEW.status USING ERRCODE = 'check_violation';
END $$;

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.period.unlock.approve', 'Approve the unlock of a locked posting period', true);
