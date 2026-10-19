-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000088's state by adding it back NOT VALID.
ALTER TABLE payments.payment_item DROP CONSTRAINT payment_item_withholding_within;
ALTER TABLE payments.payment_item ADD CONSTRAINT payment_item_withholding_within CHECK (withholding_amount = 0
  OR (sign(withholding_amount) = sign(amount) AND abs(withholding_amount) + abs(discount_amount) < abs(amount))) NOT VALID;
