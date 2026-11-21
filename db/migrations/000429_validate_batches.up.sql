-- Validates the foreign key and the check 000428 added NOT VALID (06 §6.4).
ALTER TABLE inventory.movement VALIDATE CONSTRAINT movement_batch_fkey;
ALTER TABLE inventory.movement VALIDATE CONSTRAINT movement_batch_not_in_transit;
