-- The revert is refused while a key was removed: the record of the
-- removal, which account the key named and who removed it, would be lost
-- (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM ledger.account_determination_removal) THEN
    RAISE EXCEPTION 'account determination keys were removed: reverting would lose the record of their removal'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TRIGGER account_determination_removal_deletes ON ledger.account_determination_removal;
DROP FUNCTION ledger.delete_removed_determination();
DROP TABLE ledger.account_determination_removal;
