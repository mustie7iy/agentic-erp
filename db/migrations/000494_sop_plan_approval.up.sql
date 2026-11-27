-- Approval of an S&OP plan version (S2n2; ADR-0084 As built S2n2; 03-data-model
-- §3.37): where the company's configuration names approval steps
-- (companies[].sop_approval), a planner submits a draft version for
-- approval through the workflow context's engine instead of fixing it.
-- Submitted, its consensus changes no more while its request is pending;
-- approved, it is fixed by the approver of the last step; rejected or
-- withdrawn, it is a draft again. The approval request is the workflow
-- context's, by id: no foreign key leaves the schema (02 §2.1).
--
-- Expand only (06 §6.4): a status, a column and a permission; the
-- triggers learn the new status, and a new one keeps a version from being
-- created submitted.

ALTER TABLE mrp.sop_plan
  ADD COLUMN approval_request_id uuid, -- workflow context: its latest approval request
  DROP CONSTRAINT sop_plan_status_check,
  ADD CONSTRAINT sop_plan_status_check CHECK (status IN ('draft', 'submitted', 'fixed')),
  -- A submitted version names the request it waits for.
  ADD CONSTRAINT sop_plan_submitted CHECK (status <> 'submitted' OR approval_request_id IS NOT NULL);

-- A version moves from draft to submitted or fixed, and from submitted to
-- draft (rejected or withdrawn) or fixed (approved); a fixed one changes
-- no more. It names a new approval request only as it is submitted, and
-- each submission a new one.
CREATE OR REPLACE FUNCTION mrp.sop_plan_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'S&OP plan %: a plan version is never deleted', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF OLD.status = 'fixed' THEN
    RAISE EXCEPTION 'S&OP plan %: it is fixed, so it changes no more', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF (NEW.legal_entity_id, NEW.number, NEW.as_of, NEW.first_day, NEW.last_day, NEW.months, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM (OLD.legal_entity_id, OLD.number, OLD.as_of, OLD.first_day, OLD.last_day, OLD.months, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'S&OP plan %: its company, number and horizon do not change', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF OLD.status = 'submitted' AND NEW.status = 'submitted' THEN
    RAISE EXCEPTION 'S&OP plan %: it waits for its approval, so it changes no more until it is decided', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF NEW.status = 'submitted' AND NEW.approval_request_id IS NOT DISTINCT FROM OLD.approval_request_id THEN
    RAISE EXCEPTION 'S&OP plan %: a submission names a new approval request', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id AND NEW.status <> 'submitted' THEN
    RAISE EXCEPTION 'S&OP plan %: it names a new approval request only as it is submitted', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  RETURN NEW;
END $$;

-- A version is created a draft (or fixed, as before S2n2), never
-- submitted and naming no approval request: only a submission names one
-- (batch 97S integration).
CREATE FUNCTION mrp.sop_plan_created() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = 'submitted' OR NEW.approval_request_id IS NOT NULL THEN
    RAISE EXCEPTION 'S&OP plan %: a version is created a draft naming no approval request; it is submitted afterwards', NEW.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_created';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER sop_plan_created BEFORE INSERT ON mrp.sop_plan FOR EACH ROW EXECUTE FUNCTION mrp.sop_plan_created();

-- Approving submitted S&OP plan versions: an approval permission, which
-- agents never hold (ADR-0084 As built S2n2).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('mrp.sop.approve', 'Approve submitted S&OP plan versions as the company''s consensus plan', true);
