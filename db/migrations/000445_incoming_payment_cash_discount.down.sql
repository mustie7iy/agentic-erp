-- The revert is refused while an item took a cash discount: its clearing
-- entry is posted in the ledger, its output tax corrected, and dropping
-- the column would lose which item took how much.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment_item WHERE cash_discount IS NOT NULL) THEN
    RAISE EXCEPTION 'receivables holds incoming payments whose items took a cash discount: reverting would lose them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE receivables.incoming_payment_item
  DROP CONSTRAINT incoming_payment_item_cash_discount,
  DROP COLUMN cash_discount;
