-- Period unlock (T32, 03-data-model §3.10). A locked period returns to
-- closed only through an approved period_unlock approval request. While the
-- request is pending the period names it in unlock_request_id, and only a
-- locked period can name one; the ledger clears it when the request ends
-- (T32c). The transition trigger of 000050 now lets locked -> closed
-- through only when the update consumes the named request, clearing it.
-- That the request was approved is checked by the ledger's approvals
-- callback, which closes the period under the guard unlock_request_id =
-- the approved request: the database rule stays within the ledger's
-- schema, as no context reads another's tables (02-architecture).
ALTER TABLE ledger.period_status ADD COLUMN unlock_request_id uuid; -- workflow context
ALTER TABLE ledger.period_status ADD CONSTRAINT period_status_unlock_only_locked
  CHECK (unlock_request_id IS NULL OR status = 'locked');

CREATE OR REPLACE FUNCTION ledger.check_period_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) IN (('open', 'closed'), ('closed', 'open'), ('closed', 'locked')) THEN
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) = ('locked', 'closed') AND OLD.unlock_request_id IS NOT NULL AND NEW.unlock_request_id IS NULL THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'period %/% cannot go from % to %', OLD.fiscal_year, OLD.period, OLD.status, NEW.status USING ERRCODE = 'check_violation';
END $$;

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.period.unlock.approve', 'Approve the unlock of a locked posting period', true);
