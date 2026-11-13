-- The revert is refused while an obligation names the day it is
-- satisfied: that is a person's judgement, and dropping it silently
-- would move the obligation's revenue to its delivery confirmations.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM revenue.performance_obligation WHERE satisfied_on IS NOT NULL) THEN
    RAISE EXCEPTION 'revenue.performance_obligation holds obligations with a day they are satisfied on: reverting would drop it'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE UPDATE (satisfied_on) ON revenue.performance_obligation FROM erp_app;
ALTER TABLE revenue.performance_obligation
  DROP CONSTRAINT performance_obligation_satisfied_on,
  DROP COLUMN satisfied_on;
