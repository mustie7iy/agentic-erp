-- Goods receipt items naming batches or serial numbers, which the
-- previous version cannot hold, are refused rather than dropped (06 §6.4:
-- a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.goods_movement_item WHERE batch_id IS NOT NULL OR serial_numbers IS NOT NULL) THEN
    RAISE EXCEPTION 'goods receipt items name batches or serial numbers; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE procurement.goods_movement_item
  DROP COLUMN serial_numbers,
  DROP COLUMN batch_number,
  DROP COLUMN batch_id;
