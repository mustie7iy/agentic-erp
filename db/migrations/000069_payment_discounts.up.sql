-- Cash discounts in payment runs (T36c, 03-data-model §3.10). A payment
-- item records the discount the run takes on it, of the item's sign and
-- less than its amount (a credit memo naming an invoice takes that
-- invoice's discount off its credit), fixed with the rest of the item; a
-- payment records its items' discounts, and its amount is what is paid
-- after them. Existing rows take no discount; the check holds on them, and
-- adding the columns takes the tables' locks anyway.
ALTER TABLE payments.payment_item
  ADD COLUMN discount_amount bigint NOT NULL DEFAULT 0,
  ADD CONSTRAINT payment_item_discount_within CHECK (discount_amount = 0
    OR (sign(discount_amount) = sign(amount) AND abs(discount_amount) < abs(amount)));
ALTER TABLE payments.payment ADD COLUMN discount_amount bigint NOT NULL DEFAULT 0;

-- Only is_active may change, and only from true to false; the discount is
-- fixed like the amount.
CREATE OR REPLACE FUNCTION payments.check_item_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.tenant_id, NEW.payment_id, NEW.item_fiscal_year, NEW.item_entry_id, NEW.item_line_no, NEW.amount, NEW.discount_amount, NEW.due_date)
     IS DISTINCT FROM (OLD.tenant_id, OLD.payment_id, OLD.item_fiscal_year, OLD.item_entry_id, OLD.item_line_no, OLD.amount, OLD.discount_amount, OLD.due_date)
     OR (NEW.is_active AND NOT OLD.is_active) THEN
    RAISE EXCEPTION 'payment items can only be deactivated' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
