-- The revert is refused while a payment left anything on account: its
-- credit is posted in the ledger, and dropping the columns and the
-- applications would lose which line is the credit and what applied it.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment WHERE on_account_amount <> 0)
     OR EXISTS (SELECT FROM receivables.incoming_payment_application) THEN
    RAISE EXCEPTION 'receivables holds payments on account: reverting would lose their credits and applications'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE receivables.incoming_payment_application;
ALTER TABLE receivables.incoming_payment_item DROP COLUMN application_no;
ALTER TABLE receivables.incoming_payment
  DROP CONSTRAINT incoming_payment_on_account,
  DROP COLUMN on_account_account_id,
  DROP COLUMN on_account_line_no,
  DROP COLUMN on_account_amount;
