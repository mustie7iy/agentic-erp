-- Validates the check 000088 added NOT VALID (06 §6.4): rows before it
-- withheld nothing, so it holds on them.
ALTER TABLE payments.payment_item VALIDATE CONSTRAINT payment_item_withholding_within;
