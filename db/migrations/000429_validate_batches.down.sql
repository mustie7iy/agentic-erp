-- A validated constraint cannot be marked unvalidated again: the down
-- restores 000428's state by adding both back NOT VALID.
ALTER TABLE inventory.movement
  DROP CONSTRAINT movement_batch_fkey,
  ADD CONSTRAINT movement_batch_fkey FOREIGN KEY (tenant_id, batch_id, material_id) REFERENCES inventory.batch (tenant_id, id, material_id) NOT VALID,
  DROP CONSTRAINT movement_batch_not_in_transit,
  ADD CONSTRAINT movement_batch_not_in_transit CHECK (batch_id IS NULL OR NOT in_transit) NOT VALID;
