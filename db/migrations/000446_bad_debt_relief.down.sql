-- The revert is refused while a relief is recorded: its entry is posted
-- in the ledger, its output tax recovered, and dropping the table would
-- lose which invoice and write-off it relieves.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.bad_debt_relief) THEN
    RAISE EXCEPTION 'receivables holds bad debt reliefs: reverting would lose them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE receivables.bad_debt_relief;
