DELETE FROM identity.sod_rule WHERE permission_a = 'sales.return.receive' OR permission_b = 'sales.return.receive';
DELETE FROM identity.role_permission WHERE permission_code = 'sales.return.receive';
DELETE FROM ref.permission WHERE code = 'sales.return.receive';
DROP TABLE sales.return_receipt_item;
DROP TABLE sales.return_receipt;
DROP FUNCTION sales.record_return_receipt_item();
DROP FUNCTION sales.record_return_receipt();
-- The returns order's change rule as 000303 made it. What was received
-- leaves with the receipts.
CREATE OR REPLACE FUNCTION sales.check_returns_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'returns order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'returns_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE sales.returns_order_item
  DROP CONSTRAINT returns_order_item_received,
  DROP COLUMN received_quantity;
