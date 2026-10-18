CREATE OR REPLACE FUNCTION payments.check_item_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.tenant_id, NEW.payment_id, NEW.item_fiscal_year, NEW.item_entry_id, NEW.item_line_no, NEW.amount, NEW.due_date)
     IS DISTINCT FROM (OLD.tenant_id, OLD.payment_id, OLD.item_fiscal_year, OLD.item_entry_id, OLD.item_line_no, OLD.amount, OLD.due_date)
     OR (NEW.is_active AND NOT OLD.is_active) THEN
    RAISE EXCEPTION 'payment items can only be deactivated' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE payments.payment DROP COLUMN discount_amount;
ALTER TABLE payments.payment_item DROP COLUMN discount_amount;
