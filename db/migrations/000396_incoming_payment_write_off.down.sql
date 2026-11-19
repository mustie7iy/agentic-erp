-- Reverts 000396. Refused while a payment wrote an item's difference off by
-- its reason: the previous version would show that item as paid in full
-- and lose what was written off and where.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment_item WHERE write_off_amount IS NOT NULL) THEN
    RAISE EXCEPTION 'receivables holds items written off by a reason: reverting would lose what was written off'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE receivables.incoming_payment_item DROP CONSTRAINT incoming_payment_item_write_off,
  DROP COLUMN write_off_reason_text, DROP COLUMN write_off_reason, DROP COLUMN write_off_account_id, DROP COLUMN write_off_line_no,
  DROP COLUMN write_off_amount;
