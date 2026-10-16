-- Period lock (T32, 03-data-model §3.10). A period moves open <-> closed
-- (:close, :open) and closed -> locked (:lock); nothing else changes its
-- status. Locking ends a period for good until an approved unlock request
-- returns it to closed (T32c), so the database refuses every other way out
-- of locked, and refuses locking a period still open for postings: a
-- period is closed first, which waits for the postings under way.
CREATE FUNCTION ledger.check_period_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) IN (('open', 'closed'), ('closed', 'open'), ('closed', 'locked')) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'period %/% cannot go from % to %', OLD.fiscal_year, OLD.period, OLD.status, NEW.status USING ERRCODE = 'check_violation';
END $$;

CREATE TRIGGER period_status_transition BEFORE UPDATE OF status ON ledger.period_status
  FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION ledger.check_period_transition();

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('ledger.period.lock', 'Lock posting periods and request their unlock', false);
