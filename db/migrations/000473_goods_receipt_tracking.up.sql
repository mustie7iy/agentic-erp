-- Batches and serial numbers in a purchase order's goods receipt (S2f1,
-- S2g1; ADR-0082 As built S2f1; 03-data-model §3.9). A goods movement's
-- item of a stock item names what its material document's line moved: the
-- batch of a batch-managed material, by id and number, and the serial
-- numbers of a serial-managed material's units, sorted; a reversal's item
-- names its original's. batch_id is the inventory context's batch, by id:
-- no foreign key leaves a schema but ref's (02 §2.1). Expand only (06
-- §6.4): nullable columns the previous version neither reads nor writes,
-- and checks it always passes, added NOT VALID (validated by 000474).
ALTER TABLE procurement.goods_movement_item
  ADD COLUMN batch_id uuid,     -- inventory context: inventory.batch
  ADD COLUMN batch_number text, -- inventory.batch.number, as the reader needs it beside the id
  ADD COLUMN serial_numbers text[],
  ADD CONSTRAINT goods_movement_item_batch CHECK ((batch_id IS NULL) = (batch_number IS NULL)) NOT VALID,
  ADD CONSTRAINT goods_movement_item_serial_numbers CHECK (cardinality(serial_numbers) BETWEEN 1 AND 1000) NOT VALID;
