-- Reverts 000395. Refused while a payment holds an item it translated: the
-- previous version reads every item as in the payment's currency, so it
-- would show that item's amount as its own.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment_item WHERE item_currency IS NOT NULL) THEN
    RAISE EXCEPTION 'receivables holds payments of items in another currency: reverting would show their amounts as in the payment''s currency'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE receivables.incoming_payment_item DROP CONSTRAINT incoming_payment_item_translation,
  DROP COLUMN exchange_rate, DROP COLUMN item_amount, DROP COLUMN item_currency;
