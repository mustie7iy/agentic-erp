-- The revert is refused while a payment paid another customer's item: the
-- previous version reads every item as the payment's customer's, so it
-- would show that item as the wrong customer's.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment_item t JOIN receivables.incoming_payment p ON p.tenant_id = t.tenant_id AND p.id = t.incoming_payment_id
             WHERE t.customer_id <> p.customer_id) THEN
    RAISE EXCEPTION 'receivables holds payments of several customers: reverting would show their items as the payer''s'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP INDEX receivables.incoming_payment_item_by_customer;
ALTER TABLE receivables.incoming_payment_item DROP COLUMN customer_id;
