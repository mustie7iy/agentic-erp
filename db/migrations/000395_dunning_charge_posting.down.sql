-- The revert is refused while a run has posted a charge: the previous
-- version reads no posted memo, and a cancellation of its run could no
-- longer reverse it (T76i).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM collections.dunning_charge) THEN
    RAISE EXCEPTION 'dunning runs have posted charges: reverting would lose which debit memo charges which notice' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE collections.dunning_charge;
