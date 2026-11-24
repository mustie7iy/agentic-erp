-- Validates the checks 000464 added NOT VALID (06 §6.4), scanning the
-- table without blocking writes.
ALTER TABLE procurement.goods_movement_item VALIDATE CONSTRAINT goods_movement_item_batch;
ALTER TABLE procurement.goods_movement_item VALIDATE CONSTRAINT goods_movement_item_serial_numbers;
