-- Accepting intercompany differences and finalizing consolidation runs
-- (T68g; ADR-0059 §5, §8, §10; 03-data-model §3.24): an open difference
-- of a computed run is accepted by a person, with who, when and why; it
-- moves only open -> accepted, nothing else of it changes, and its
-- version moves with it. A run's finalization and supersession use the
-- columns, transitions and one-final index of 000224. accepted_by is the
-- identity context's principal: no foreign key leaves the schema (02
-- §2.1). The permissions to accept and to finalize. Expand only: a new
-- status, new nullable columns and a new trigger, which the previous
-- version neither reads nor writes.
ALTER TABLE consolidation.run_difference
  ADD COLUMN accepted_by uuid,             -- identity context
  ADD COLUMN accepted_at timestamptz,
  ADD COLUMN reason      text,
  ADD COLUMN version     integer NOT NULL DEFAULT 1;

ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_status_check;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_status_check
  CHECK (status IN ('matched', 'within_tolerance', 'open', 'accepted', 'outside_group', 'no_trading_partner'));
-- An accepted difference is the open pair it was.
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_pair;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_pair CHECK (CASE WHEN status IN ('matched', 'within_tolerance', 'open', 'accepted')
  THEN partner_amount IS NOT NULL AND tolerance IS NOT NULL AND difference IS NOT DISTINCT FROM amount + partner_amount
  ELSE partner_amount IS NULL AND difference IS NULL AND tolerance IS NULL END);
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_status;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_status CHECK (CASE WHEN status = 'matched' THEN difference = 0
  WHEN status = 'within_tolerance' THEN difference <> 0 AND abs(difference) <= tolerance WHEN status IN ('open', 'accepted') THEN abs(difference) > tolerance
  ELSE true END);
-- Who accepted it, when and why, exactly when it is accepted.
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_accepted CHECK ((status = 'accepted') = (accepted_by IS NOT NULL)
  AND (accepted_by IS NULL) = (accepted_at IS NULL) AND (accepted_by IS NULL) = (reason IS NULL)
  AND (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 2000));
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_version CHECK (version >= 1);

-- Accepting a difference is its one change.
GRANT UPDATE (status, accepted_by, accepted_at, reason, version) ON consolidation.run_difference TO erp_app;

-- A difference is written by its run's matching and never accepted
-- there; it moves only open -> accepted, while its run is computed, its
-- version one up, and nothing else of it changes. An insert in the tenant
-- app.importing names (ADR-0029, 000230) keeps the status it carries.
CREATE FUNCTION consolidation.check_run_difference() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  st text;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status = 'accepted' AND NOT platform.importing(NEW.tenant_id) THEN
      RAISE EXCEPTION 'an intercompany difference is accepted after its run is computed, not written accepted'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'run_difference_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) IS DISTINCT FROM ('open', 'accepted') OR NEW.version <> OLD.version + 1
      OR (NEW.tenant_id, NEW.id, NEW.run_id, NEW.line_no, NEW.elimination_code, NEW.legal_entity_id, NEW.partner_id, NEW.amount, NEW.partner_amount,
          NEW.difference, NEW.tolerance) IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.run_id, OLD.line_no, OLD.elimination_code, OLD.legal_entity_id,
          OLD.partner_id, OLD.amount, OLD.partner_amount, OLD.difference, OLD.tolerance) THEN
    RAISE EXCEPTION 'intercompany difference % moves only from open to accepted, its version one up, and keeps its figures', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_difference_transition';
  END IF;
  SELECT status INTO st FROM consolidation.run WHERE tenant_id = NEW.tenant_id AND id = NEW.run_id;
  IF st IS DISTINCT FROM 'computed' THEN
    RAISE EXCEPTION 'consolidation run % is %, not computed: its differences are accepted while it is computed', NEW.run_id, st
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_difference_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION consolidation.check_run_difference() OWNER TO erp_owner;
CREATE TRIGGER run_difference_transition BEFORE INSERT OR UPDATE ON consolidation.run_difference
  FOR EACH ROW EXECUTE FUNCTION consolidation.check_run_difference();

-- Accepting differences and finalizing runs (ADR-0059 §12).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('consolidation.difference.accept', 'Accept the open intercompany differences of the consolidation runs of the groups whose companies one may act in',
   false),
  ('consolidation.run.finalize', 'Finalize the consolidation runs of the groups whose companies one may act in as their consolidated figures', false);
