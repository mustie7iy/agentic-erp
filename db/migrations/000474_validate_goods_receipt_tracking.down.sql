-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000473's state by adding them back NOT VALID.
ALTER TABLE procurement.goods_movement_item
  DROP CONSTRAINT goods_movement_item_batch,
  ADD CONSTRAINT goods_movement_item_batch CHECK ((batch_id IS NULL) = (batch_number IS NULL)) NOT VALID,
  DROP CONSTRAINT goods_movement_item_serial_numbers,
  ADD CONSTRAINT goods_movement_item_serial_numbers CHECK (cardinality(serial_numbers) BETWEEN 1 AND 1000) NOT VALID;
