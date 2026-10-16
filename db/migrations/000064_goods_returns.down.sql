DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.goods_movement WHERE movement_type IN ('122', '123')) THEN
    RAISE EXCEPTION 'returns to the supplier are recorded; they cannot be undone by a migration';
  END IF;
END $$;
DROP TRIGGER goods_movement_item_sign ON procurement.goods_movement_item;
DROP TRIGGER goods_movement_reversal_type ON procurement.goods_movement;
DROP FUNCTION procurement.check_movement_item_sign();
DROP FUNCTION procurement.check_movement_reversal();
ALTER TABLE procurement.goods_movement DROP CONSTRAINT goods_movement_return_reason, DROP COLUMN reason,
  DROP CONSTRAINT goods_movement_reversal_pair,
  ADD CONSTRAINT goods_movement_check CHECK ((movement_type = '102') = (reversal_of_id IS NOT NULL)),
  DROP CONSTRAINT goods_movement_movement_type_check,
  ADD CONSTRAINT goods_movement_movement_type_check CHECK (movement_type IN ('101', '102'));
