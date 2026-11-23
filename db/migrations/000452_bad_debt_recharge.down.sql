-- The revert is refused while an adjustment is recorded: its entry is
-- posted in the ledger, the tax charged again, and dropping the table
-- would lose which invoice and payment it charges.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.bad_debt_recharge) THEN
    RAISE EXCEPTION 'receivables holds bad debt relief charged again: reverting would lose it'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE receivables.bad_debt_recharge;
